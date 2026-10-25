package steps

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Drive the real boundary with disposable Git ancestry and forge responses.
func TestAccountedForgeHeadCoreBoundaries(t *testing.T) {
	for _, step := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush} {
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
				h.facts.HeadSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
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
					selected.ForgeHeadSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
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
				c.PRTarget.ForgeHeadSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
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
			if err != nil || outcome == nil || outcome.RestartFrom != types.StepReview {
				t.Fatalf("correction failed to reach normal re-review: %+v %v", outcome, err)
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
