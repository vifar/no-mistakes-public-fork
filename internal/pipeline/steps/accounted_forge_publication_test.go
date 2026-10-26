package steps

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The observable contract is publication, not just the comparison helper:
// an accounted catch-up keeps its generation and writes an exact-head
// attestation before publishing; a real foreign sibling publishes nothing.
func TestAccountedForgeHeadPublicationBoundary(t *testing.T) {
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
