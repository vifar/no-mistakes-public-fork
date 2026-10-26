package steps

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Drive the real boundary with disposable Git ancestry and forge responses.
func TestAccountedForgeHeadCoreBoundaries(t *testing.T) {
	for _, step := range []types.StepName{types.StepRebase, types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush} {
		for _, kind := range []string{"catch-up", "ancestor", "simultaneous fix", "foreign", "unaccounted local", "wrong base"} {
			t.Run(string(step)+"/"+kind, func(t *testing.T) {
				dir, base, head := setupGitRepo(t)
				c := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
				selected := pipeline.PRTargetSelection{PRURL: "https://github.com/test/repo/pull/42", SourceRepo: "test/repo", SourceBranch: "feature", TargetBranch: "main", ForgeHeadSHA: base}
				if _, err := guardPRContextWithSelection(c, types.StepRebase, selected, false); err != nil {
					t.Fatal(err)
				}
				before, _ := c.DB.GetRunPRContext(c.Run.ID)
				observed := head
				wantErr := kind == "foreign" || kind == "unaccounted local" || kind == "wrong base"
				if kind == "ancestor" || kind == "simultaneous fix" || kind == "unaccounted local" {
					gitCmd(t, dir, "commit", "--allow-empty", "-m", "local fix")
					local := gitCmd(t, dir, "rev-parse", "HEAD")
					c.Run.HeadSHA = local
					if err := c.DB.UpdateRunHeadSHA(c.Run.ID, local); err != nil {
						t.Fatal(err)
					}
					if kind == "simultaneous fix" || kind == "ancestor" {
						c.PRContextAfterStep = true
						if err := recordPipelineHeadRange(c, head, local); err != nil {
							t.Fatal(err)
						}
						if kind == "simultaneous fix" {
							observed = local
						}
					}
					if kind == "unaccounted local" {
						observed = local
					}
				}
				if kind == "foreign" {
					gitCmd(t, dir, "checkout", "-b", "foreign", base)
					gitCmd(t, dir, "commit", "--allow-empty", "-m", "foreign")
					observed = gitCmd(t, dir, "rev-parse", "HEAD")
					gitCmd(t, dir, "checkout", "feature")
				}
				liveBase := "main"
				if kind == "wrong base" {
					liveBase = "other"
					gitCmd(t, dir, "branch", "other", base)
				}
				facts := fmt.Sprintf(`{"number":42,"html_url":%q,"state":"open","head":{"ref":"feature","sha":%q,"repo":{"full_name":"test/repo"}},"base":{"ref":%q}}`, selected.PRURL, observed, liveBase)
				bin := fakeCLIBinDir(t)
				linkTestBinary(t, bin, "gh")
				c.Env = fakeCLIEnv(bin, map[string]string{"FAKE_CLI_MODE": "ci-gh-reconcile", "FAKE_CLI_PR_FACTS_JSON": facts})
				decision, err := GuardPRContext(c, step)
				after, _ := c.DB.GetRunPRContext(c.Run.ID)
				t.Logf("pinned=%s observed=%s local=%s generations=%d->%d decision=%+v err=%v", before.ForgeHeadSHA, observed, c.Run.HeadSHA, before.Generation, after.Generation, decision, err)
				if (err != nil) != wantErr {
					t.Fatalf("guard error=%v wantError=%t", err, wantErr)
				}
				if !wantErr {
					if decision.RestartFrom != "" || after.ForgeHeadSHA != observed {
						t.Fatalf("receipt=%+v decision=%+v", after, decision)
					}
					if kind != "simultaneous fix" && kind != "ancestor" && after.Generation != before.Generation {
						t.Fatal("forge-only correction advanced comparison generation")
					}
				}
			})
		}
	}
}

func TestAccountedForgeHeadAttestation(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprint(foreign), func(t *testing.T) {
			c, h, pr := mutationComparisonFixture(t)
			receipt, _ := c.DB.GetRunPRContext(c.Run.ID)
			stale := receipt.PRContextCandidate
			stale.ForgeHeadSHA = c.Run.BaseSHA
			if _, err := c.DB.BindRunPRContext(c.Run.ID, stale, types.StepRebase); err != nil {
				t.Fatal(err)
			}
			before, _ := c.DB.GetRunPRContext(c.Run.ID)
			if foreign {
				h.facts.HeadSHA = foreignForgeHead(t, c)
			}
			err := verifyPRMutationComparison(c, h, pr, c.Run.HeadSHA, true)
			after, _ := c.DB.GetRunPRContext(c.Run.ID)
			t.Logf("attestation pinned=%s observed=%s local=%s generation=%d->%d err=%v", before.ForgeHeadSHA, h.facts.HeadSHA, c.Run.HeadSHA, before.Generation, after.Generation, err)
			if (err != nil) != foreign {
				t.Fatalf("attestation=%v wantForeignRefusal=%t", err, foreign)
			}
			if !foreign && (after.ForgeHeadSHA != c.Run.HeadSHA || after.Generation != before.Generation) {
				t.Fatalf("receipt=%+v", after)
			}
		})
	}
}

func TestAccountedForgeHeadTargetRewinds(t *testing.T) {
	for _, pending := range []bool{false, true} {
		for _, foreign := range []bool{false, true} {
			t.Run(fmt.Sprintf("pending=%t/foreign=%t", pending, foreign), func(t *testing.T) {
				c, selected := rewindFixture(t)
				selected.PRURL = "https://github.com/test/repo/pull/42"
				selected.SourceRepo = "test/repo"
				selected.SourceBranch = "feature"
				selected.ForgeHeadSHA = c.Run.BaseSHA
				if _, err := guardPRContextWithSelection(c, types.StepRebase, selected, false); err != nil {
					t.Fatal(err)
				}
				advanceRewindTarget(t, c)
				if pending {
					if _, err := guardPRContextWithSelection(c, types.StepTest, selected, false); err != nil {
						t.Fatal(err)
					}
				}
				before, _ := c.DB.GetRunPRContext(c.Run.ID)
				selected.ForgeHeadSHA = c.Run.HeadSHA
				if foreign {
					selected.ForgeHeadSHA = foreignForgeHead(t, c)
				}
				decision, err := guardPRContextWithSelection(c, types.StepTest, selected, false)
				after, _ := c.DB.GetRunPRContext(c.Run.ID)
				t.Logf("rewind pinned=%s observed=%s local=%s generation=%d->%d pending=%t decision=%+v err=%v", before.ForgeHeadSHA, selected.ForgeHeadSHA, c.Run.HeadSHA, before.Generation, after.Generation, pending, decision, err)
				if (err != nil) != foreign {
					t.Fatalf("rewind=%v wantForeignRefusal=%t", err, foreign)
				}
				if !foreign {
					if decision.RestartFrom != types.StepRebase {
						t.Fatalf("decision=%+v", decision)
					}
					if pending && after.Generation != before.Generation {
						t.Fatal("forge refresh reclassified pending rewind")
					}
				}
			})
		}
	}
}

func TestAccountedForgeHeadPreservesTestOwnerClaim(t *testing.T) {
	claim := pendingReviewTestClaim("true")
	c, receipt := completedReviewSupportFixture(t, claim)
	stale := receipt.PRContextCandidate
	stale.ForgeHeadSHA = c.Run.BaseSHA
	if _, err := c.DB.BindRunPRContext(c.Run.ID, stale, types.StepRebase); err != nil {
		t.Fatal(err)
	}
	// The initial stale observation predates Review and its owner evidence.
	row, err := c.DB.InsertStepResult(c.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := types.MarshalFindingsJSON(Findings{Items: []Finding{claim}})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.DB.SetStepFindings(row.ID, encoded); err != nil {
		t.Fatal(err)
	}
	if err = c.DB.CompleteReviewStep(row.ID, c.Run.ID, c.Run.HeadSHA, 0, 1, ""); err != nil {
		t.Fatal(err)
	}
	c.PRContext, err = c.DB.GetRunPRContext(c.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := *c.PRContext
	exit := 0
	proofs, err := resolveTestReviewSupport(c, "true", &exit, c.Run.HeadSHA, "2026-10-25T21:00:00Z")
	if err != nil || len(proofs) != 1 {
		t.Fatalf("proofs=%+v err=%v", proofs, err)
	}
	selection := pipeline.PRTargetSelection{PRURL: before.PRURL, SourceRepo: before.SourceRepo, SourceBranch: before.SourceBranch, TargetBranch: before.TargetBranch, ForgeHeadSHA: c.Run.HeadSHA}
	decision, err := guardPRContextWithSelection(c, types.StepDocument, selection, false)
	after, _ := c.DB.GetRunPRContext(c.Run.ID)
	claims, _, claimErr := currentReviewSupportClaims(c, types.FindingClaimTest)
	t.Logf("claim=%s owner=%s pinned=%s observed=%s local=%s generation=%d->%d", types.ReviewSupportClaimID(claim), proofs[0].ID, before.ForgeHeadSHA, selection.ForgeHeadSHA, c.Run.HeadSHA, before.Generation, after.Generation)
	if err != nil || decision.RestartFrom != "" || claimErr != nil || len(claims) != 1 {
		t.Fatalf("decision=%+v err=%v claims=%+v claimErr=%v", decision, err, claims, claimErr)
	}
	if after.Generation != proofs[0].Support.OwnerResult.Generation || types.ReviewSupportClaimID(claims[0]) != proofs[0].ID {
		t.Fatal("existing owner evidence became stale or relabeled")
	}
}

func TestAccountedForgeHeadPushCorrection(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprint(foreign), func(t *testing.T) {
			c, _, _ := unpublishedAttestationFixture(t)
			receipt, err := c.DB.GetRunPRContext(c.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			stale := receipt.PRContextCandidate
			stale.ForgeHeadSHA = c.Run.BaseSHA
			if _, err = c.DB.BindRunPRContext(c.Run.ID, stale, types.StepRebase); err != nil {
				t.Fatal(err)
			}
			recordReviewApproval(t, c, c.Run.HeadSHA)
			c.PRTarget = &pipeline.PRTargetSelection{PRURL: receipt.PRURL, SourceRepo: receipt.SourceRepo, SourceBranch: receipt.SourceBranch, TargetBranch: receipt.TargetBranch, ForgeHeadSHA: c.Run.HeadSHA}
			if foreign {
				c.PRTarget.ForgeHeadSHA = foreignForgeHead(t, c)
			}
			if err = os.WriteFile(filepath.Join(c.WorkDir, "correction.txt"), []byte("correction\n"), 0600); err != nil {
				t.Fatal(err)
			}
			outcome, err := (&PushStep{}).Execute(c)
			t.Logf("push pinned=%s observed=%s local=%s outcome=%+v err=%v", stale.ForgeHeadSHA, c.PRTarget.ForgeHeadSHA, c.Run.HeadSHA, outcome, err)
			if foreign {
				if err == nil {
					t.Fatal("foreign forge head accepted while committing")
				}
				return
			}
			if err != nil || outcome == nil || outcome.NeedsApproval || outcome.RestartFrom != "" {
				t.Fatalf("accounted correction failed the existing local-target push path: %+v %v", outcome, err)
			}
			// This fixture's push target is local. Preserve its existing policy
			// rather than importing #101's separate GitHub re-review behavior.
			published := gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
			target := resolvePushURL(c)
			if got := gitCmd(t, target, "rev-parse", "refs/heads/feature"); got != published {
				t.Fatalf("published head=%s want=%s", got, published)
			}
			run, readErr := c.DB.GetRun(c.Run.ID)
			if readErr != nil || run.LastPushedSHA == nil || *run.LastPushedSHA != published {
				t.Fatalf("publication binding=%+v err=%v", run, readErr)
			}
		})
	}
}

func TestAccountedForgeHeadDurableOwnPush(t *testing.T) {
	c, h, pr := mutationComparisonFixture(t)
	old, _ := c.DB.GetRunPRContext(c.Run.ID)
	gitCmd(t, c.WorkDir, "checkout", "-b", "old-published", c.Run.BaseSHA)
	gitCmd(t, c.WorkDir, "commit", "--allow-empty", "-m", "previous own publication")
	published := gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
	gitCmd(t, c.WorkDir, "checkout", "feature")
	if err := c.DB.UpdateRunPushBinding(c.Run.ID, db.PushBinding{HeadSHA: published}); err != nil {
		t.Fatal(err)
	}
	h.facts.HeadSHA = published
	if err := verifyPRMutationComparison(c, h, pr, c.Run.HeadSHA, true); err != nil {
		t.Fatalf("durable own head rejected: %v", err)
	}
	after, _ := c.DB.GetRunPRContext(c.Run.ID)
	if after.ForgeHeadSHA != published || after.Generation != old.Generation {
		t.Fatalf("receipt=%+v", after)
	}
	t.Logf("own publication pinned=%s observed=%s local=%s generation=%d->%d", old.ForgeHeadSHA, published, c.Run.HeadSHA, old.Generation, after.Generation)
}

func TestAccountedForgeHeadWithTargetAndReviewFix(t *testing.T) {
	c, selected := rewindFixture(t)
	selected.PRURL = "https://github.com/test/repo/pull/42"
	selected.SourceRepo = "test/repo"
	selected.SourceBranch = "feature"
	selected.ForgeHeadSHA = c.Run.BaseSHA
	if _, err := guardPRContextWithSelection(c, types.StepRebase, selected, false); err != nil {
		t.Fatal(err)
	}
	before, _ := c.DB.GetRunPRContext(c.Run.ID)
	gitCmd(t, c.WorkDir, "commit", "--allow-empty", "-m", "Review fix")
	head := gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
	if err := recordPipelineHeadRange(c, c.Run.HeadSHA, head); err != nil {
		t.Fatal(err)
	}
	c.Run.HeadSHA = head
	if err := c.DB.UpdateRunHeadSHA(c.Run.ID, head); err != nil {
		t.Fatal(err)
	}
	c.PRContextAfterStep = true
	selected.ForgeHeadSHA = head
	advanceRewindTarget(t, c)
	decision, err := guardPRContextWithSelection(c, types.StepReview, selected, false)
	after, _ := c.DB.GetRunPRContext(c.Run.ID)
	t.Logf("target+fix pinned=%s observed=%s local=%s generations=%d->%d decision=%+v err=%v", before.ForgeHeadSHA, head, head, before.Generation, after.Generation, decision, err)
	if err != nil || decision.RestartFrom != types.StepRebase {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
}

// foreignForgeHead constructs a real sibling, rather than relying on a missing
// object being rejected. The worktree is restored before the guard executes.
func foreignForgeHead(t *testing.T, c *pipeline.StepContext) string {
	t.Helper()
	gitCmd(t, c.WorkDir, "checkout", "-b", "foreign-forge", c.Run.BaseSHA)
	gitCmd(t, c.WorkDir, "commit", "--allow-empty", "-m", "foreign publisher")
	head := gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
	gitCmd(t, c.WorkDir, "checkout", "feature")
	return head
}

func accountedRewindFixture(t *testing.T) (*pipeline.StepContext, pipeline.PRTargetSelection) {
	t.Helper()
	c, selected := rewindFixture(t)
	remote := c.Repo.UpstreamURL
	c.Repo.UpstreamURL = "https://github.com/test/repo"
	gitCmd(t, c.WorkDir, "config", "url."+remote+".insteadOf", c.Repo.UpstreamURL)
	selected.PRURL = "https://github.com/test/repo/pull/42"
	selected.SourceRepo, selected.SourceBranch = "test/repo", "feature"
	selected.ForgeHeadSHA = c.Run.BaseSHA
	if _, err := guardPRContextWithSelection(c, types.StepRebase, selected, false); err != nil {
		t.Fatal(err)
	}
	c.PRTarget = &selected
	setAccountedForgeFacts(t, c, selected)
	return c, selected
}

func setAccountedForgeFacts(t *testing.T, c *pipeline.StepContext, selected pipeline.PRTargetSelection) {
	t.Helper()
	bin := fakeCLIBinDir(t)
	linkTestBinary(t, bin, "gh")
	facts := fmt.Sprintf(`{"number":42,"html_url":%q,"state":"open","head":{"ref":%q,"sha":%q,"repo":{"full_name":%q}},"base":{"ref":%q}}`, selected.PRURL, selected.SourceBranch, selected.ForgeHeadSHA, selected.SourceRepo, selected.TargetBranch)
	c.Env = fakeCLIEnv(bin, map[string]string{"FAKE_CLI_MODE": "ci-gh-reconcile", "FAKE_CLI_PR_FACTS_JSON": facts})
}

func TestAccountedForgeHeadAfterRebaseKeepsBothLocalProofs(t *testing.T) {
	for _, pending := range []bool{false, true} {
		for _, observation := range []string{"pinned", "rebased", "foreign"} {
			t.Run(fmt.Sprintf("pending=%t/%s", pending, observation), func(t *testing.T) {
				c, selected := accountedRewindFixture(t)
				original := c.Run.HeadSHA
				foreign := foreignForgeHead(t, c)
				target := advanceRewindTarget(t, c)
				if pending {
					if decision, err := GuardPRContext(c, types.StepTest); err != nil || decision.RestartFrom != types.StepRebase {
						t.Fatalf("classify rewind: %+v %v", decision, err)
					}
					if out, err := (&RebaseStep{}).Execute(c); err != nil || out == nil || out.NeedsApproval {
						t.Fatalf("integrate rewind: %+v %v", out, err)
					}
				} else {
					gitCmd(t, c.WorkDir, "rebase", target)
					c.Run.HeadSHA = gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
					if err := c.DB.UpdateRunHeadSHA(c.Run.ID, c.Run.HeadSHA); err != nil {
						t.Fatal(err)
					}
				}
				if isAncestor(c.Ctx, c.WorkDir, original, c.Run.HeadSHA) {
					t.Fatal("fixture must rewrite the pinned local head")
				}
				selected.ForgeHeadSHA = map[string]string{"pinned": original, "rebased": c.Run.HeadSHA, "foreign": foreign}[observation]
				setAccountedForgeFacts(t, c, selected)
				before, err := c.DB.GetRunPRContext(c.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				c.PRContextAfterStep, c.PRContextRebaseCompleted = true, true
				decision, err := GuardPRContext(c, types.StepRebase)
				after, readErr := c.DB.GetRunPRContext(c.Run.ID)
				if readErr != nil {
					t.Fatal(readErr)
				}
				t.Logf("post-rebase pinned=%s observed=%s pinned-local=%s local=%s generation=%d->%d decision=%+v err=%v", before.ForgeHeadSHA, selected.ForgeHeadSHA, before.LocalHeadSHA, c.Run.HeadSHA, before.Generation, after.Generation, decision, err)
				if observation == "foreign" {
					if err == nil || !strings.Contains(err.Error(), "unaccounted pull request head") || after.PRContextCandidate != before.PRContextCandidate || after.Generation != before.Generation {
						t.Fatalf("foreign head changed receipt: %+v %v", after, err)
					}
					return
				}
				if err != nil || after.LocalHeadSHA != c.Run.HeadSHA || after.ForgeHeadSHA != selected.ForgeHeadSHA {
					t.Fatalf("accounted rewrite rejected: %+v %v", after, err)
				}
				if pending && decision.RestartFrom != "" || !pending && decision.RestartFrom != types.StepRebase {
					t.Fatalf("unexpected comparison transition: %+v", decision)
				}
				p, err := c.DB.GetPendingTargetRewind(c.Run.ID)
				if err != nil || (p != nil) != !pending {
					t.Fatalf("pending=%+v err=%v", p, err)
				}
			})
		}
	}
}

func TestAccountedForgeHeadCatchUpAtRewindIntegration(t *testing.T) {
	for _, strategy := range []string{config.RebaseStrategyRebase, config.RebaseStrategyMerge} {
		for _, fixing := range []bool{false, true} {
			for _, drift := range []string{"catch-up", "foreign", "source", "base", "remote source"} {
				t.Run(fmt.Sprintf("%s/fixing=%t/%s", strategy, fixing, drift), func(t *testing.T) {
					c, selected := accountedRewindFixture(t)
					c.Config.Rebase.Strategy = strategy
					target := advanceRewindTarget(t, c)
					if _, err := GuardPRContext(c, types.StepTest); err != nil {
						t.Fatal(err)
					}
					if _, err := GuardPRContext(c, types.StepRebase); err != nil {
						t.Fatal(err)
					}
					before, err := c.DB.GetRunPRContext(c.Run.ID)
					if err != nil {
						t.Fatal(err)
					}
					selected.ForgeHeadSHA = c.Run.HeadSHA
					switch drift {
					case "foreign":
						selected.ForgeHeadSHA = foreignForgeHead(t, c)
					case "source":
						selected.SourceBranch = "other"
					case "base":
						selected.TargetBranch = "other"
						gitCmd(t, c.WorkDir, "push", "origin", "main:other")
					case "remote source":
						foreign := foreignForgeHead(t, c)
						gitCmd(t, c.WorkDir, "push", "--force", "origin", foreign+":feature")
					}
					setAccountedForgeFacts(t, c, selected)
					c.Fixing = fixing
					out, err := (&RebaseStep{}).Execute(c)
					after, readErr := c.DB.GetRunPRContext(c.Run.ID)
					if readErr != nil {
						t.Fatal(readErr)
					}
					t.Logf("integration pinned=%s observed=%s local=%s generation=%d->%d outcome=%+v err=%v", before.ForgeHeadSHA, selected.ForgeHeadSHA, before.LocalHeadSHA, before.Generation, after.Generation, out, err)
					if drift != "catch-up" {
						cause := map[string]string{
							"foreign":       "unaccounted pull request head",
							"source":        "on other differs",
							"base":          "local head or PR identity changed during pending rewind",
							"remote source": "remote source changed during pending rewind",
						}[drift]
						if err == nil || !strings.Contains(err.Error(), cause) || gitCmd(t, c.WorkDir, "rev-parse", "HEAD") != before.LocalHeadSHA {
							t.Fatalf("unsafe integration accepted: %+v %v", out, err)
						}
						return
					}
					if err != nil || out == nil || out.NeedsApproval || after.Generation != before.Generation || after.ForgeHeadSHA != selected.ForgeHeadSHA {
						t.Fatalf("accounted catch-up refused: %+v %+v %v", out, after, err)
					}
					gitCmd(t, c.WorkDir, "merge-base", "--is-ancestor", target, "HEAD")
					p, err := c.DB.GetPendingTargetRewind(c.Run.ID)
					if err != nil || p == nil || p.ForgeHeadSHA == nil || *p.ForgeHeadSHA != selected.ForgeHeadSHA || p.Generation != before.Generation {
						t.Fatalf("pending observation=%+v %v", p, err)
					}
				})
			}
		}
	}
}

func TestAccountedForgeHeadRewindAfterLeftoverCommit(t *testing.T) {
	for _, strategy := range []string{config.RebaseStrategyRebase, config.RebaseStrategyMerge} {
		for _, refreshAt := range []string{"integration", "completion"} {
			t.Run(strategy+"/"+refreshAt, func(t *testing.T) {
				c, selected := accountedRewindFixture(t)
				c.Config.Rebase.Strategy = strategy
				target := advanceRewindTarget(t, c)
				if _, err := GuardPRContext(c, types.StepTest); err != nil {
					t.Fatal(err)
				}
				p, err := c.DB.GetPendingTargetRewind(c.Run.ID)
				if err != nil || p == nil {
					t.Fatalf("pending=%+v %v", p, err)
				}
				selected.ForgeHeadSHA = c.Run.HeadSHA
				if refreshAt == "integration" {
					setAccountedForgeFacts(t, c, selected)
				}
				if err := os.WriteFile(filepath.Join(c.WorkDir, "feature.txt"), []byte("preserved validation edit\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if out, err := (&RebaseStep{}).Execute(c); err != nil || out == nil || out.NeedsApproval {
					t.Fatalf("leftover integration: %+v %v", out, err)
				}
				before, err := c.DB.GetRunPRContext(c.Run.ID)
				if err != nil || before.Generation != p.Generation+1 {
					t.Fatalf("leftover did not advance receipt: %+v %v", before, err)
				}
				setAccountedForgeFacts(t, c, selected)
				c.PRContextAfterStep, c.PRContextRebaseCompleted = true, true
				decision, err := GuardPRContext(c, types.StepRebase)
				after, readErr := c.DB.GetRunPRContext(c.Run.ID)
				if readErr != nil {
					t.Fatal(readErr)
				}
				t.Logf("leftover pinned=%s observed=%s pinned-local=%s local=%s pending-generation=%d receipt=%d->%d decision=%+v err=%v", before.ForgeHeadSHA, selected.ForgeHeadSHA, before.LocalHeadSHA, c.Run.HeadSHA, p.Generation, before.Generation, after.Generation, decision, err)
				if err != nil || decision.RestartFrom != "" || after.ForgeHeadSHA != selected.ForgeHeadSHA || after.LocalHeadSHA != c.Run.HeadSHA {
					t.Fatalf("leftover completion: %+v %+v %v", after, decision, err)
				}
				if p, err := c.DB.GetPendingTargetRewind(c.Run.ID); err != nil || p != nil {
					t.Fatalf("pending=%+v %v", p, err)
				}
				if got := gitCmd(t, c.WorkDir, "show", "HEAD:feature.txt"); got != "preserved validation edit" {
					t.Fatalf("validation edit lost: %q", got)
				}
				gitCmd(t, c.WorkDir, "merge-base", "--is-ancestor", target, "HEAD")
			})
		}
	}
}
