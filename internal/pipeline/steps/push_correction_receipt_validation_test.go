package steps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The R8 contract requires every live-target check to accept before Push binds
// its correction. Refusal or interruption preserves the commit, but neither
// may replace the comparison, its pin, or its pending rewind observation.
func TestPushStep_CorrectionReceiptWaitsForTargetValidation(t *testing.T) {
	t.Parallel()
	for _, provider := range []string{"local", "github"} {
		for _, refusal := range []string{"rewritten target", "exhausted budget", "unreadable target", "interrupted observation", "valid target"} {
			if provider == "local" && refusal == "valid target" {
				// Local publication without a forge is covered by
				// TestPushStep_FormatterCommitAtThePublicationBoundaryIsThePipelineOwn.
				continue
			}
			t.Run(provider+"/"+refusal, func(t *testing.T) {
				var c *pipeline.StepContext
				var upstream string
				if provider == "github" {
					c, _, upstream = unpublishedAttestationFixture(t)
					gitCmd(t, c.WorkDir, "remote", "set-url", "origin", c.Repo.UpstreamURL)
					c.Repo.URLsVerified = true
				} else {
					c, _ = publicationFixture(t)
					upstream = c.Repo.UpstreamURL
				}
				receipt, err := c.DB.GetRunPRContext(c.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				stale := receipt.PRContextCandidate
				stale.PRURL, stale.SourceRepo, stale.SourceBranch = "https://github.com/test/repo/pull/42", "test/repo", "feature"
				stale.ForgeHeadSHA = c.Run.BaseSHA
				if _, err := c.DB.BindRunPRContext(c.Run.ID, stale, types.StepRebase); err != nil {
					t.Fatal(err)
				}
				recordReviewApproval(t, c, c.Run.HeadSHA)
				selected := &pipeline.PRTargetSelection{}
				selected.PRURL, selected.SourceRepo, selected.SourceBranch = stale.PRURL, stale.SourceRepo, stale.SourceBranch
				selected.TargetBranch, selected.ForgeHeadSHA = stale.TargetBranch, c.Run.HeadSHA
				c.PRTarget = selected
				c.Env = append(c.Env, "FAKE_CLI_PR_FACTS_JSON="+mutationPRFactsJSON("open", selected.ForgeHeadSHA, "main"))
				var observationInterrupted <-chan bool
				var cancelObservation context.CancelFunc
				switch refusal {
				case "rewritten target":
					gitCmd(t, c.WorkDir, "checkout", "main")
					gitCmd(t, c.WorkDir, "commit", "--amend", "--allow-empty", "-m", "rewritten target")
					gitCmd(t, c.WorkDir, "push", "--force", "origin", "main")
					gitCmd(t, c.WorkDir, "checkout", "feature")
				case "exhausted budget":
					advanceRewindTarget(t, c)
					c.Config.Rebase.MaxTargetAdvanceRewinds = 0
				case "unreadable target":
					gitCmd(t, c.WorkDir, "--git-dir="+upstream, "update-ref", "-d", "refs/heads/main")
				case "interrupted observation":
					// Cancel during the real target fetch, after Push committed
					// its correction. The upload-pack child blocks until Git's
					// context cancellation stops its process group.
					// Use origin's resolved local URL for Git while retaining
					// the registered GitHub identity for the live forge read.
					c.Repo.URLsVerified = false
					marker := filepath.Join(t.TempDir(), "fetch-started")
					uploadPack := filepath.Join(t.TempDir(), "upload-pack")
					if err := os.WriteFile(uploadPack, []byte("#!/bin/sh\n: > "+shellQuotePreparation(marker)+"\nexec sleep 30\n"), 0700); err != nil {
						t.Fatal(err)
					}
					gitCmd(t, c.WorkDir, "config", "remote.origin.uploadpack", shellQuotePreparation(uploadPack))
					ctx, cancel := context.WithCancel(c.Ctx)
					defer cancel()
					c.Ctx = ctx
					cancelObservation = cancel
					interrupted := make(chan bool, 1)
					observationInterrupted = interrupted
					go func() {
						ticker := time.NewTicker(time.Millisecond)
						defer ticker.Stop()
						for {
							select {
							case <-ctx.Done():
								interrupted <- false
								return
							case <-ticker.C:
								if _, err := os.Stat(marker); err == nil {
									cancel()
									interrupted <- true
									return
								}
							}
						}
					}()
				}
				before, err := c.DB.GetRunPRContext(c.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(c.WorkDir, "feature.txt"), []byte("preserved Push correction\n"), 0600); err != nil {
					t.Fatal(err)
				}
				outcome, pushErr := (&PushStep{}).Execute(c)
				if cancelObservation != nil {
					cancelObservation()
					if !<-observationInterrupted || !errors.Is(pushErr, context.Canceled) {
						t.Fatalf("target fetch was not interrupted: %v", pushErr)
					}
				}
				after, err := c.DB.GetRunPRContext(c.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				pending, err := c.DB.GetPendingTargetRewind(c.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				head := gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
				remote := gitCmd(t, c.WorkDir, "--git-dir="+upstream, "rev-parse", "refs/heads/feature")
				t.Logf("provider=%s refusal=%s pinned=%s->%s observed=%s local=%s->%s actual=%s remote=%s pin=%q->%q generation=%d->%d pending=%+v outcome=%+v err=%v", provider, refusal, before.ForgeHeadSHA, after.ForgeHeadSHA, selected.ForgeHeadSHA, before.LocalHeadSHA, after.LocalHeadSHA, head, remote, before.PinnedLocalHeadSHA, after.PinnedLocalHeadSHA, before.Generation, after.Generation, pending, outcome, pushErr)
				if got := gitCmd(t, c.WorkDir, "show", "HEAD:feature.txt"); got != "preserved Push correction" {
					t.Fatalf("correction content lost: %q", got)
				}
				if refusal == "valid target" {
					if pushErr != nil || outcome == nil || after.LocalHeadSHA != head || after.ForgeHeadSHA != selected.ForgeHeadSHA || after.Generation != before.Generation+1 || after.PinnedLocalHeadSHA != before.LocalHeadSHA || pending != nil {
						t.Fatalf("validated correction was not bound: receipt=%+v outcome=%+v err=%v", after, outcome, pushErr)
					}
					if outcome.RestartFrom != types.StepReview || remote != before.LocalHeadSHA {
						t.Fatalf("GitHub correction did not return to Review: %+v remote=%s", outcome, remote)
					}
					return
				}
				if pushErr == nil || head == before.LocalHeadSHA || remote != before.LocalHeadSHA || *after != *before || pending != nil {
					t.Fatalf("refused or interrupted Push must retain correction without binding or publishing it: receipt=%+v pending=%+v outcome=%+v err=%v", after, pending, outcome, pushErr)
				}
			})
		}
	}
}
