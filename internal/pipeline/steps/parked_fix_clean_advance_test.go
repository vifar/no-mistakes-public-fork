package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestAnsweredFixRoundSurvivesCleanTargetAdvance covers the crew-tooling lane
// case (crew run 01M5W1055, 2026-10-26, issue #237): main advanced while an
// answered Review fix round was queued, the run rebased and restarted Review
// before the fix ran, and the reviewer re-raised the same findings as
// unimplemented, losing a whole review cycle each time.
//
// A clean target advance (the head still merges without conflict and its
// merge base is unchanged) is not drift. The answered fix round must run at
// the reviewed head before Review looks again, with no rewind and no rebase.
func TestAnsweredFixRoundSurvivesCleanTargetAdvance(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	c := newTestContextWithDBRecords(t, nil, dir, base, head, config.Commands{})
	c.Repo.UpstreamURL = dir
	c.Config.AutoFix = config.AutoFix{Review: 0}
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var sequence []string
	record := func(event string) {
		mu.Lock()
		defer mu.Unlock()
		sequence = append(sequence, event)
	}
	reviews := 0
	c.Agent = &mockAgent{name: "test", runFn: func(ctx context.Context, o agent.RunOpts) (*agent.Result, error) {
		if strings.Contains(o.Prompt, "Investigate previous review findings") {
			record("fix")
			if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature code\nfixed\n"), 0o644); err != nil {
				return nil, err
			}
			return &agent.Result{Output: []byte(`{"summary":"fix reported issue","verification":"passed"}`)}, nil
		}
		record("review")
		reviews++
		f := cleanReviewFindings()
		if reviews == 1 {
			f.Items = []types.Finding{{ID: "review-1", Severity: types.FindingSeverityError, Action: types.ActionAutoFix, File: "feature.txt", Line: 1, Description: "needs a fix", Support: &types.FindingSupport{ClaimType: types.FindingClaimSource, Source: &types.FindingSourceSupport{Path: "feature.txt", Line: 1, Quote: "feature code"}}}}
		}
		raw, _ := json.Marshal(f)
		return &agent.Result{Output: raw}, nil
	}}

	// The target advances in a separate checkout, so the run's own worktree
	// is never touched: a new file on main that cannot conflict with feature.
	targetCheckout := filepath.Join(t.TempDir(), "main")
	advanced := ""
	advanceTarget := func() {
		gitCmd(t, dir, "worktree", "add", targetCheckout, "main")
		if err := os.WriteFile(filepath.Join(targetCheckout, "unrelated.txt"), []byte("later target\n"), 0o644); err != nil {
			t.Error(err)
			return
		}
		gitCmd(t, targetCheckout, "add", "unrelated.txt")
		gitCmd(t, targetCheckout, "commit", "-m", "advance main cleanly")
		advanced = gitCmd(t, targetCheckout, "rev-parse", "HEAD")
	}

	enrollExecutorGate(t, p, c.Repo.ID)
	var ex *pipeline.Executor
	parks := 0
	responseDone := make(chan error, 1)
	ex = pipeline.NewExecutor(c.DB, p, c.Config, c.Agent, []pipeline.Step{&RebaseStep{}, &ReviewStep{}}, func(e ipc.Event) {
		if e.StepName == nil || *e.StepName != types.StepReview || e.Status == nil ||
			(*e.Status != string(types.StepStatusAwaitingApproval) && *e.Status != string(types.StepStatusFixReview)) {
			return
		}
		parks++
		if parks != 1 {
			return
		}
		// The operator answers fix; main advances cleanly before the
		// fix round starts.
		go func() {
			advanceTarget()
			responseDone <- ex.Respond(types.StepReview, types.ActionFix, []string{"review-1"}, gateSnapshot(ex))
		}()
	})
	ex.SetPRContextGuard(GuardPRContext)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := ex.Execute(ctx, c.Run, c.Repo, dir)
	if parks == 0 {
		t.Fatalf("Review never parked for an answer: err=%v", err)
	}
	if respErr := <-responseDone; respErr != nil {
		t.Fatalf("fix response refused: %v", respErr)
	}
	if advanced == "" || advanced == base {
		t.Fatalf("main did not advance: %q", advanced)
	}

	mu.Lock()
	got := append([]string(nil), sequence...)
	mu.Unlock()
	if len(got) < 2 || got[0] != "review" || got[1] != "fix" {
		t.Fatalf("the answered fix round did not run before Review looked again: sequence=%v err=%v", got, err)
	}
	run, readErr := c.DB.GetRun(c.Run.ID)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if run.TargetAdvanceRewinds != 0 {
		t.Fatalf("a clean target advance rewound the run %d time(s); sequence=%v", run.TargetAdvanceRewinds, got)
	}
	if err != nil {
		t.Fatalf("run failed after the answered fix round: %v (sequence=%v)", err, got)
	}
}
