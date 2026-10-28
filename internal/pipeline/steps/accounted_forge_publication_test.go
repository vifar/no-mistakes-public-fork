package steps

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The observable contract is publication, not just the comparison helper:
// an accounted catch-up keeps its generation and writes an exact-head
// attestation before publishing; a real foreign sibling publishes nothing.
func TestAccountedForgeHeadPublicationBoundary(t *testing.T) {
	t.Parallel()
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprintf("foreign=%t", foreign), func(t *testing.T) {
			f, logFile := publicationAttestationFixture(t)
			// Keep forge discovery active while every Git transport resolves
			// to the disposable bare upstream rather than the real forge.
			gitCmd(t, f.dir, "config", "url."+f.upstream+".insteadOf", "https://github.com/test/repo")
			gitCmd(t, f.dir, "remote", "set-url", "origin", "https://github.com/test/repo")
			before, err := f.sctx.DB.GetRunPRContext(f.sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			stale := before.PRContextCandidate
			stale.ForgeHeadSHA = f.sctx.Run.BaseSHA
			if _, err := f.sctx.DB.BindRunPRContext(f.sctx.Run.ID, stale, types.StepRebase); err != nil {
				t.Fatal(err)
			}
			recordReviewApproval(t, f.sctx, f.headSHA)
			bodyFile, ok := envValue(f.sctx.Env, "FAKE_CLI_PR_BODY_FILE")
			if !ok {
				t.Fatal("fixture has no PR body file")
			}
			if err := os.WriteFile(bodyFile, []byte(compliantPipelineBody(t, stale.ForgeHeadSHA)), 0o600); err != nil {
				t.Fatal(err)
			}
			before, err = f.sctx.DB.GetRunPRContext(f.sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			observed := f.headSHA
			if foreign {
				observed = foreignForgeHead(t, f.sctx)
			}
			f.sctx.Env = append(f.sctx.Env, "FAKE_CLI_PR_FACTS_JSON="+mutationPRFactsJSON("open", observed, "main"))
			t.Logf("publication setup provider=%s branch=%s default=%s upstream=%s owned=%s", resolvedProvider(f.sctx), f.sctx.Run.Branch, f.sctx.Repo.DefaultBranch, f.sctx.Repo.UpstreamURL, runPRURL(f.sctx))
			outcome, err := (&PushStep{}).Execute(f.sctx)
			after, readErr := f.sctx.DB.GetRunPRContext(f.sctx.Run.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			t.Logf("publication pinned=%s observed=%s local=%s target=%s generation=%d->%d remote=%s outcome=%+v err=%v\n%s", before.ForgeHeadSHA, observed, f.localHead(t), before.TargetSHA, before.Generation, after.Generation, f.remoteHead(t), outcome, err, f.log())
			if foreign {
				if err == nil || !strings.Contains(err.Error(), "unaccounted pull request head") || after.PRContextCandidate != before.PRContextCandidate {
					t.Fatalf("foreign head was accepted: receipt=%+v err=%v", after, err)
				}
				if f.remoteHead(t) != f.headSHA {
					t.Fatal("foreign observation changed the remote")
				}
				log, readErr := os.ReadFile(logFile)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if strings.Contains(string(log), "pr edit") {
					t.Fatalf("foreign observation wrote a PR attestation: %s", log)
				}
				return
			}
			if err != nil || outcome == nil || outcome.NeedsApproval || outcome.RestartFrom != "" {
				t.Fatalf("accounted catch-up prevented publication: %+v %v", outcome, err)
			}
			if after.Generation != before.Generation || after.ForgeHeadSHA != observed || f.remoteHead(t) != f.headSHA {
				t.Fatalf("forge-only catch-up changed comparison or publication: %+v", after)
			}
			body := readFakeGHBodyArg(t, logFile)
			attestation := parsePipelineAttestationForTest(t, body)
			if attestation.HeadSHA != f.headSHA {
				t.Fatalf("attestation head=%s want=%s", attestation.HeadSHA, f.headSHA)
			}
			t.Logf("published PR body:\n%s", body)
		})
	}
}

func TestAccountedForgeHeadPRAndCIGuards(t *testing.T) {
	t.Parallel()
	for _, step := range []types.StepName{types.StepPR, types.StepCI} {
		for _, foreign := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/foreign=%t", step, foreign), func(t *testing.T) {
				c, selected := accountedRewindFixture(t)
				before, err := c.DB.GetRunPRContext(c.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				selected.ForgeHeadSHA = c.Run.HeadSHA
				if foreign {
					selected.ForgeHeadSHA = foreignForgeHead(t, c)
				}
				setAccountedForgeFacts(t, c, selected)
				decision, err := GuardPRContext(c, step)
				after, readErr := c.DB.GetRunPRContext(c.Run.ID)
				if readErr != nil {
					t.Fatal(readErr)
				}
				t.Logf("%s pinned=%s observed=%s local=%s target=%s generation=%d->%d decision=%+v err=%v", step, before.ForgeHeadSHA, selected.ForgeHeadSHA, c.Run.HeadSHA, before.TargetSHA, before.Generation, after.Generation, decision, err)
				if (err != nil) != foreign || after.Generation != before.Generation {
					t.Fatalf("guard decision=%+v receipt=%+v err=%v", decision, after, err)
				}
				if foreign {
					if after.PRContextCandidate != before.PRContextCandidate {
						t.Fatal("foreign observation changed the receipt")
					}
				} else if decision.RestartFrom != "" || after.ForgeHeadSHA != selected.ForgeHeadSHA {
					t.Fatalf("accounted catch-up was not preserved: %+v %+v", decision, after)
				}
			})
		}
	}
}

func TestAccountedForgeHeadPRGuardCatchesUpToPublishedAncestor(t *testing.T) {
	t.Parallel()
	for _, afterStep := range []bool{false, true} {
		for _, kind := range []string{"catch-up", "unchanged ancestor", "exact", "foreign", "source", "base", "local", "merged", "closed"} {
			t.Run(fmt.Sprintf("after=%t/%s", afterStep, kind), func(t *testing.T) {
				c, selected := accountedRewindFixture(t)
				ancestor := c.Run.HeadSHA
				gitCmd(t, c.WorkDir, "commit", "--allow-empty", "-m", "published descendant")
				published := gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
				c.Run.HeadSHA = published
				if err := c.DB.UpdateRunHeadSHA(c.Run.ID, published); err != nil {
					t.Fatal(err)
				}
				if _, err := guardPRContextWithSelection(c, types.StepRebase, selected, false); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, c.WorkDir, "push", "origin", "feature")
				if err := c.DB.UpdateRunPushBinding(c.Run.ID, db.PushBinding{HeadSHA: published}); err != nil {
					t.Fatal(err)
				}
				recordReviewApproval(t, c, published)
				before, err := c.DB.GetRunPRContext(c.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				c.PRContext = before
				selected.ForgeHeadSHA = ancestor
				state := "open"
				cause := ""
				switch kind {
				case "unchanged ancestor":
					selected.ForgeHeadSHA = before.ForgeHeadSHA
				case "exact":
					selected.ForgeHeadSHA = published
				case "foreign":
					selected.ForgeHeadSHA = foreignForgeHead(t, c)
					cause = "unaccounted pull request head"
				case "source":
					selected.SourceBranch = "other"
					cause = "on other differs"
				case "base":
					selected.TargetBranch = "other"
					gitCmd(t, c.WorkDir, "push", "origin", "main:other")
					cause = "differs from local head"
				case "local":
					gitCmd(t, c.WorkDir, "commit", "--allow-empty", "-m", "unattributed local edit")
					cause = "PR comparison changed after"
				case "merged", "closed":
					state = kind
					cause = "is " + state
				}
				setAccountedForgeFacts(t, c, selected)
				wireState := state
				if state == "merged" {
					wireState = "closed"
				}
				facts := fmt.Sprintf(`{"number":42,"html_url":%q,"state":%q,"merged":%t,"merge_commit_sha":%q,"head":{"ref":%q,"sha":%q,"repo":{"full_name":%q}},"base":{"ref":%q}}`, selected.PRURL, wireState, state == "merged", published, selected.SourceBranch, selected.ForgeHeadSHA, selected.SourceRepo, selected.TargetBranch)
				c.Env = append(c.Env, "FAKE_CLI_PR_FACTS_JSON="+facts)
				c.PRContextAfterStep = afterStep
				decision, err := GuardPRContext(c, types.StepPR)
				after, readErr := c.DB.GetRunPRContext(c.Run.ID)
				if readErr != nil {
					t.Fatal(readErr)
				}
				t.Logf("PR guard after=%t pinned=%s observed=%s local=%s published=%s target=%s generation=%d->%d decision=%+v err=%v", afterStep, before.ForgeHeadSHA, selected.ForgeHeadSHA, gitCmd(t, c.WorkDir, "rev-parse", "HEAD"), published, before.TargetSHA, before.Generation, after.Generation, decision, err)
				if after.Generation != before.Generation || after.LocalHeadSHA != published || after.TargetSHA != before.TargetSHA {
					t.Fatalf("forge observation changed comparison: %+v", after)
				}
				if cause != "" {
					if err == nil || !strings.Contains(err.Error(), cause) {
						t.Fatalf("expected %q refusal, got %+v %v", cause, decision, err)
					}
					if kind != "local" && after.PRContextCandidate != before.PRContextCandidate {
						t.Fatalf("refused observation changed receipt: %+v", after)
					}
					return
				}
				if err != nil || decision.RestartFrom != "" || after.ForgeHeadSHA != selected.ForgeHeadSHA || c.PRContext.ForgeHeadSHA != selected.ForgeHeadSHA {
					t.Fatalf("accounted ancestor rejected: %+v %+v %v", decision, after, err)
				}
				if !isAncestor(c.Ctx, c.WorkDir, selected.ForgeHeadSHA, published) {
					t.Fatal("accepted observation is not a published ancestor")
				}
				if !afterStep {
					c.PRContextAfterStep = true
					decision, err = GuardPRContext(c, types.StepPR)
					if err != nil || decision.RestartFrom != "" {
						t.Fatalf("post-step guard refused the refreshed observation: %+v %v", decision, err)
					}
				}
			})
		}
	}
}

func TestAccountedForgeHeadPRGuardKeepsTheAcceptedPreRebaseObservation(t *testing.T) {
	t.Parallel()
	for _, afterStep := range []bool{false, true} {
		t.Run(fmt.Sprintf("after=%t", afterStep), func(t *testing.T) {
			c, selected := accountedRewindFixture(t)
			selected.ForgeHeadSHA = c.Run.HeadSHA
			setAccountedForgeFacts(t, c, selected)
			if _, err := GuardPRContext(c, types.StepTest); err != nil {
				t.Fatal(err)
			}
			advanceRewindTarget(t, c)
			if decision, err := GuardPRContext(c, types.StepTest); err != nil || decision.RestartFrom != types.StepRebase {
				t.Fatalf("classify rewind: %+v %v", decision, err)
			}
			if out, err := (&RebaseStep{}).Execute(c); err != nil || out == nil || out.NeedsApproval {
				t.Fatalf("integrate rewind: %+v %v", out, err)
			}
			c.PRContextAfterStep, c.PRContextRebaseCompleted = true, true
			if decision, err := GuardPRContext(c, types.StepRebase); err != nil || decision.RestartFrom != "" {
				t.Fatalf("complete rewind: %+v %v", decision, err)
			}
			if isAncestor(c.Ctx, c.WorkDir, selected.ForgeHeadSHA, c.Run.HeadSHA) {
				t.Fatal("fixture must rewrite the already accounted head")
			}
			gitCmd(t, c.WorkDir, "push", "--force", "origin", "feature")
			if err := c.DB.UpdateRunPushBinding(c.Run.ID, db.PushBinding{HeadSHA: c.Run.HeadSHA}); err != nil {
				t.Fatal(err)
			}
			recordReviewApproval(t, c, c.Run.HeadSHA)
			before, err := c.DB.GetRunPRContext(c.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			c.PRContextAfterStep, c.PRContextRebaseCompleted = afterStep, false
			decision, err := GuardPRContext(c, types.StepPR)
			after, readErr := c.DB.GetRunPRContext(c.Run.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			t.Logf("PR after rebase pinned=%s observed=%s local=%s generation=%d->%d decision=%+v err=%v", before.ForgeHeadSHA, selected.ForgeHeadSHA, c.Run.HeadSHA, before.Generation, after.Generation, decision, err)
			if err != nil || decision.RestartFrom != "" || after.PRContextCandidate != before.PRContextCandidate || after.Generation != before.Generation {
				t.Fatalf("accepted observation lost after rebase: %+v %+v %v", decision, after, err)
			}
		})
	}
}
