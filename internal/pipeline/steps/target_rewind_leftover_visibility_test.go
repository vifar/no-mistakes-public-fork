package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestLeftoverCorrectionPreservesHiddenUntrackedFiles(t *testing.T) {
	for _, boundary := range []string{"test", "document", "rewind"} {
		t.Run(boundary, func(t *testing.T) {
			var c *pipeline.StepContext
			if boundary == "rewind" {
				var selected pipeline.PRTargetSelection
				c, selected = rewindFixture(t)
				advanceRewindTarget(t, c)
				c.PRContextAfterStep = true
				decision, err := guardPRContextWithSelection(c, types.StepTest, selected, false)
				if err != nil || decision.RestartFrom != types.StepRebase {
					t.Fatalf("target advance did not rewind: %+v %v", decision, err)
				}
				c.PRContextAfterStep = false
			} else {
				c = documentFixture(t)
			}
			inheritHiddenUntracked(t, c.WorkDir)
			head := c.Run.HeadSHA
			const content = "package main\n"
			writeEdit := func() error {
				return os.WriteFile(filepath.Join(c.WorkDir, "new_test.go"), []byte(content), 0o600)
			}
			var step pipeline.Step
			switch boundary {
			case "test":
				c.Agent = &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
					if err := writeEdit(); err != nil {
						return nil, err
					}
					return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
				}}
				step = &TestStep{}
			case "document":
				step = &DocumentStep{}
			case "rewind":
				step = &RebaseStep{}
			}
			if boundary != "test" {
				if err := writeEdit(); err != nil {
					t.Fatal(err)
				}
				if status := gitCmd(t, c.WorkDir, "status", "--porcelain"); status != "" {
					t.Fatalf("fixture did not hide the untracked edit: %q", status)
				}
			}
			outcome, err := step.Execute(c)
			if err != nil || outcome == nil || outcome.NeedsApproval {
				t.Fatalf("leftover correction failed: %+v %v", outcome, err)
			}
			if boundary != "rewind" && outcome.RestartFrom != types.StepReview {
				t.Fatalf("correction did not request re-review: %+v", outcome)
			}
			if got := gitCmd(t, c.WorkDir, "show", "HEAD:new_test.go"); got != "package main" {
				t.Fatalf("the untracked edit was not committed: %q", got)
			}
			if got := gitCmd(t, c.WorkDir, "rev-parse", "HEAD"); got == head || c.Run.HeadSHA != got {
				t.Fatalf("run head did not follow the correction: before=%s head=%s run=%s", head, got, c.Run.HeadSHA)
			}
			if allowed, err := c.DB.CanCommitLeftoverValidationEdits(c.Run.ID); err != nil || allowed {
				t.Fatalf("correction budget was not spent: allowed=%v err=%v", allowed, err)
			}
			if status := gitCmd(t, c.WorkDir, "status", "--porcelain", "--untracked-files=all"); status != "" {
				t.Fatalf("correction left edits behind: %q", status)
			}
		})
	}
}
