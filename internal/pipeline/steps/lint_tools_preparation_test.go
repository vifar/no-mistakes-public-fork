package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

// The emitted prompt is an agent interface: the issue #212 contract requires
// an installed package-local binary, rather than a missing-tool plan assembled
// before preparation. The subsequent check must actually invoke that binary.
func TestLintStep_PreparationInstallsToolBeforeStandaloneAndHousekeepingPrompts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX preparation script")
	}
	t.Parallel()
	for _, mode := range []string{"standalone", "housekeeping"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA, _ := lintMonorepo(t, false)
			gitCmd(t, dir, "config", "extensions.worktreeConfig", "true")
			calls := 0
			ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				calls++
				if _, err := os.Stat(filepath.Join(dir, "controller/node_modules/.bin/prettier")); err != nil {
					t.Fatalf("agent ran before preparation installed prettier: %v", err)
				}
				if !strings.Contains(opts.Prompt, "prettier (configured by controller/package.json): run from `controller` with `./node_modules/.bin/prettier`; files: package.json, src/a.ts") {
					t.Fatalf("emitted prompt did not resolve the newly installed package tool:\n%s", opts.Prompt)
				}
				payload := strings.Replace(falseMissingPrettier, `"severity":"warning"`, `"category":"lint","severity":"warning"`, 1)
				return &agent.Result{Output: json.RawMessage(payload)}, nil
			}}
			prepare := `mkdir -p controller/node_modules/.bin
cat > controller/node_modules/.bin/prettier <<'TOOL'
#!/bin/sh
printf '%s\n' "$PWD" "$@" > node_modules/.bin/verified
TOOL
chmod +x controller/node_modules/.bin/prettier`
			sctx := newHousekeepingContext(t, ag, dir, baseSHA, headSHA, config.Commands{Prepare: prepare})
			sctx.GateDir = t.TempDir()
			gitCmd(t, sctx.GateDir, "init", "--bare")
			if mode == "housekeeping" {
				if _, err := (&DocumentStep{}).Execute(sctx); err != nil {
					t.Fatal(err)
				}
			}
			outcome, err := (&LintStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || outcome.NeedsApproval {
				t.Fatalf("agent calls=%d outcome=%+v, want one call and a verified clean result", calls, outcome)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "controller/node_modules/.bin/verified"))
			if err != nil {
				t.Fatalf("prepared tool was not checked: %v", err)
			}
			lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
			if len(lines) != 5 || filepath.Base(lines[0]) != "controller" || strings.Join(lines[1:], " ") != "--check --ignore-unknown ./package.json ./src/a.ts" {
				t.Fatalf("verification invocation=%q, want package directory and its own changed files", raw)
			}
		})
	}
}

func TestLintStep_ConfiguredCommandSuccessDoesNotInvokeAgent(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Fatal("successful configured lint must not invoke an agent")
		return nil, nil
	}}
	sctx := newHousekeepingContext(t, ag, dir, baseSHA, headSHA, config.Commands{Lint: "echo configured-lint-success"})
	outcome, err := (&LintStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval || outcome.ExitCode != 0 || len(ag.calls) != 0 {
		t.Fatalf("outcome=%+v agent calls=%d, want configured-command success", outcome, len(ag.calls))
	}
}
