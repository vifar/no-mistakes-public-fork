package citest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
)

// ciCallBudgetFixture monitors a PR whose only check stays pending for an
// hour of simulated time and returns every gh invocation the monitor made and
// the waits between its polls.
//
// Measured on this fixture (issue: GitHub rate limits stalled ~10 lanes on
// 2026-10-24): 308 gh calls before (a 30/60/120s ladder, five `gh pr view`
// per poll), 67 after (unchanged polls back off to 5m; one combined view per
// poll plus the closing head re-read).
func ciCallBudgetFixture(t *testing.T) ([]string, []time.Duration) {
	t.Helper()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
	pending := `[{"name":"build","state":"IN_PROGRESS","bucket":"pending"}]`
	logPath := filepath.Join(t.TempDir(), "gh.log")
	env := append(stepstest.FakeCIGH(t, "OPEN", pending), "FAKE_CLI_LOG="+logPath, "FAKE_CLI_PR_HEAD_SHA="+headSHA)

	prURL := "https://github.com/test/repo/pull/42"
	sctx := stepstest.NewTestContext(t, &stepstest.MockAgent{AgentName: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = time.Hour

	now := time.Unix(1700000000, 0)
	var waits []time.Duration
	step := (&steps.CIStep{}).SetNow(func() time.Time { return now }).
		SetWaitForNextPoll(func(ctx context.Context, interval time.Duration) error {
			waits = append(waits, interval)
			now = now.Add(interval)
			return nil
		})
	if _, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 0); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line != "" {
			calls = append(calls, line)
		}
	}
	return calls, waits
}

func TestCIStep_UnchangedPendingCIBacksOffItsPolling(t *testing.T) {
	t.Parallel()
	calls, waits := ciCallBudgetFixture(t)
	counts := map[string]int{}
	for _, call := range calls {
		fields := strings.Fields(call)
		counts[strings.Join(fields[:min(2, len(fields))], " ")]++
	}
	t.Logf("gh calls over one hour of unchanged pending CI: %d %v; polls: %d", len(calls), counts, len(waits)+1)
	if len(waits) == 0 || waits[0] != 30*time.Second {
		t.Fatalf("waits=%v, want the first wait on the normal 30s cadence", waits)
	}
	for i := 1; i < len(waits); i++ {
		if waits[i] < waits[i-1] && i != len(waits)-1 {
			t.Fatalf("waits=%v, want unchanged polls never to speed up", waits)
		}
		if waits[i] > 5*time.Minute {
			t.Fatalf("waits=%v, want the backoff capped at 5m", waits)
		}
	}
	if len(calls) > 80 {
		t.Fatalf("%d gh calls for one hour of unchanged CI, want at most 80 (308 before the backoff and the per-poll view)", len(calls))
	}
	polls := len(waits) + 1
	if counts["pr view"] > 2*polls+2 {
		t.Fatalf("%d gh pr view calls over %d polls, want one combined view plus the closing head re-read per poll", counts["pr view"], polls)
	}
}

func TestCIStep_ChangingChecksKeepTheNormalCadence(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
	sequence := []string{
		`[{"name":"build","state":"IN_PROGRESS","bucket":"pending"}]`,
		`[{"name":"build","state":"IN_PROGRESS","bucket":"pending"}]`,
		`[{"name":"build","state":"IN_PROGRESS","bucket":"pending"},{"name":"lint","state":"IN_PROGRESS","bucket":"pending"}]`,
		`[{"name":"build","state":"SUCCESS","bucket":"pass"},{"name":"lint","state":"IN_PROGRESS","bucket":"pending"}]`,
		`[{"name":"build","state":"SUCCESS","bucket":"pass"},{"name":"lint","state":"IN_PROGRESS","bucket":"pending"},{"name":"test","state":"IN_PROGRESS","bucket":"pending"}]`,
	}
	env := append(stepstest.FakeCIGHSequence(t, "OPEN", sequence), "FAKE_CLI_PR_HEAD_SHA="+headSHA)
	prURL := "https://github.com/test/repo/pull/42"
	sctx := stepstest.NewTestContext(t, &stepstest.MockAgent{AgentName: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = time.Hour

	now := time.Unix(1700000000, 0)
	var waits []time.Duration
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx
	step := (&steps.CIStep{}).SetNow(func() time.Time { return now }).
		SetWaitForNextPoll(func(ctx context.Context, interval time.Duration) error {
			waits = append(waits, interval)
			now = now.Add(interval)
			if len(waits) == 4 {
				cancel()
				return ctx.Err()
			}
			return nil
		})
	_, _ = stepstest.ExecuteWithAutoFix(t, step, sctx, 0)
	want := []time.Duration{30 * time.Second, 60 * time.Second, 30 * time.Second, 30 * time.Second}
	if len(waits) != len(want) {
		t.Fatalf("waits=%v, want %v", waits, want)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("waits=%v, want %v: one unchanged poll backs off, any change resets it", waits, want)
		}
	}
}

// Issue #226: the backoff is for polls that read a healthy, settled state that
// did not change. A poll whose read failed, or whose readiness proof failed,
// is not "nothing changed": backing it off delays noticing the recovery.
func TestCIStep_UnhealthyPollsKeepTheNormalCadence(t *testing.T) {
	for _, tc := range []struct {
		name      string
		env       func(t *testing.T, head string) []string
		configure func(*pipeline.StepContext, *steps.CIStep)
	}{
		{name: "mergeability read fails", env: func(t *testing.T, head string) []string {
			pending := `[{"name":"build","state":"IN_PROGRESS","bucket":"pending"}]`
			return append(stepstest.FakeCIGHMergeableError(t, "OPEN", pending, "gh: mergeability unavailable"), "FAKE_CLI_PR_HEAD_SHA="+head)
		}},
		{name: "readiness comparison fails", env: func(t *testing.T, head string) []string {
			// Green checks with no bound comparison receipt: every poll's
			// readiness proof fails while the forge reads stay identical.
			green := `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`
			return append(stepstest.FakeCIGH(t, "OPEN", green), "FAKE_CLI_PR_HEAD_SHA="+head)
		}},
		{name: "empty rollup waiting for registration", env: func(t *testing.T, head string) []string {
			return append(stepstest.FakeCIGH(t, "OPEN", `[]`), "FAKE_CLI_PR_HEAD_SHA="+head)
		}},
		{name: "empty rollup with missing required check", env: func(t *testing.T, head string) []string {
			return append(stepstest.FakeCIGH(t, "OPEN", `[]`), "FAKE_CLI_PR_HEAD_SHA="+head)
		}, configure: func(sctx *pipeline.StepContext, _ *steps.CIStep) {
			sctx.Config.CI.MustPass = []string{"build"}
		}},
		{name: "timeout base tip read fails", env: func(t *testing.T, head string) []string {
			return append(stepstest.FakeCIGH(t, "OPEN", `[{"name":"build","state":"IN_PROGRESS","bucket":"pending"}]`), "FAKE_CLI_PR_HEAD_SHA="+head)
		}, configure: func(_ *pipeline.StepContext, step *steps.CIStep) {
			step.SetBaseBranchTip(func(context.Context) (string, bool) { return "fallback-tip", false })
		}},
		{name: "behind base tip read fails", env: func(t *testing.T, head string) []string {
			return append(stepstest.FakeCIGHMergeable(t, "OPEN", `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`, "MERGEABLE BEHIND"), "FAKE_CLI_PR_HEAD_SHA="+head)
		}, configure: func(sctx *pipeline.StepContext, step *steps.CIStep) {
			sctx.Config.CITimeout = -1
			sctx.Config.CI.RebaseBehind = true
			step.SetBaseBranchTip(func(context.Context) (string, bool) { return "fallback-tip", false })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
			prURL := "https://github.com/test/repo/pull/42"
			sctx := stepstest.NewTestContext(t, &stepstest.MockAgent{AgentName: "test"}, dir, baseSHA, headSHA, config.Commands{})
			sctx.Env = tc.env(t, headSHA)
			sctx.Run.PRURL = &prURL
			sctx.Config.CITimeout = time.Hour
			sctx.Config.CIReadinessReceiptTimeout = time.Hour

			now := time.Unix(1700000000, 0)
			var waits []time.Duration
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sctx.Ctx = ctx
			step := (&steps.CIStep{}).SetBaseBranchTip(func(context.Context) (string, bool) { return baseSHA, true }).SetNow(func() time.Time { return now }).
				SetWaitForNextPoll(func(ctx context.Context, interval time.Duration) error {
					waits = append(waits, interval)
					now = now.Add(interval)
					if len(waits) == 4 {
						cancel()
						return ctx.Err()
					}
					return nil
				})
			if tc.configure != nil {
				tc.configure(sctx, step)
			}
			_, _ = stepstest.ExecuteWithAutoFix(t, step, sctx, 0)
			if len(waits) != 4 {
				t.Fatalf("waits=%v, want four polls", waits)
			}
			for _, wait := range waits {
				if wait != 30*time.Second {
					t.Fatalf("waits=%v, want every unhealthy poll on the normal 30s cadence", waits)
				}
			}
		})
	}
}

func TestCIStep_RegistrationWaitCadenceAcrossMergeability(t *testing.T) {
	for _, mergeability := range []string{"MERGEABLE", "UNKNOWN", "PENDING"} {
		for _, required := range []bool{false, true} {
			for _, noCI := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/required=%t/no_ci=%t", mergeability, required, noCI), func(t *testing.T) {
					t.Parallel()
					dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
					sctx := stepstest.NewTestContextWithDBRecords(t, &stepstest.MockAgent{AgentName: "test"}, dir, baseSHA, headSHA, config.Commands{})
					sctx.Env = stepstest.FakeCIGHMergeable(t, "OPEN", `[]`, mergeability)
					stepstest.BindCurrentPRComparison(t, sctx, "https://github.com/test/repo/pull/42", "test/repo", "main")
					sctx.Config.CITimeout = time.Hour
					sctx.Config.NoCI = noCI
					if required {
						sctx.Config.CI.MustPass = []string{"build"}
					}
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					sctx.Ctx = ctx
					now := time.Unix(1700000000, 0)
					var waits []time.Duration
					step := (&steps.CIStep{}).
						SetBaseBranchTip(func(context.Context) (string, bool) { return baseSHA, true }).
						SetNow(func() time.Time { return now }).
						SetWaitForNextPoll(func(ctx context.Context, interval time.Duration) error {
							waits = append(waits, interval)
							now = now.Add(interval)
							if len(waits) == 4 {
								cancel()
								return ctx.Err()
							}
							return nil
						})
					_, _ = step.Execute(sctx)
					want := []time.Duration{30 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second}
					if noCI && !required {
						want = []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second, 240 * time.Second}
					}
					if len(waits) != len(want) {
						t.Fatalf("waits=%v, want %v", waits, want)
					}
					for i := range want {
						if waits[i] != want[i] {
							t.Fatalf("waits=%v, want %v: registration waits retain normal cadence; trusted no_ci only waives absent checks without explicit requirements", waits, want)
						}
					}
				})
			}
		}
	}
}
