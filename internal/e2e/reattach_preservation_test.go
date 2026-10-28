//go:build e2e

package e2e

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The issue #190 acceptance contract forbids replacing existing launch values.
// Drive the CLI against a real queued push run, including a conflicting owner
// paired with a new intent, which must not partially adopt either value.
func TestReattachPreservesExistingLaunchValues(t *testing.T) {
	t.Setenv("NO_MISTAKES_OWNER", "")
	h := newReattachHarness(t, "daemon:\n  max_concurrent_runs: 1\n  hold_slot_while_parked: true\n")
	h.CommitChange("blocker", "parkme.txt", "hold\n", "blocker")
	out, err := h.Run("axi", "run", "--intent", "Hold the slot", "--wait", "2m")
	if err != nil {
		t.Fatalf("blocker: %v\n%s", err, out)
	}
	waitForStepStatus(t, h, "blocker", types.StepReview, types.StepStatusAwaitingApproval, 2*time.Minute)
	h.CommitChange("feature/preserve", "adopt.txt", "preserve\n", "preserve")
	h.PushToGate("feature/preserve")
	run := h.ActiveRun("feature/preserve")
	if run == nil || run.Status != types.RunQueued || run.Owner != "" {
		t.Fatalf("expected ownerless queued push run, got %+v", run)
	}

	check := func(owner, intent, source string) {
		t.Helper()
		if got := h.RunInfo(run.ID).Owner; got != owner {
			t.Fatalf("owner = %q, want %q", got, owner)
		}
		got := readRunIntent(t, h.NMHome, run.ID)
		if intent == "" {
			if got.summary != nil && strings.TrimSpace(*got.summary) != "" {
				t.Fatalf("unexpected intent: %q", *got.summary)
			}
			return
		}
		if got.summary == nil || *got.summary != intent || got.source == nil || *got.source != source || got.score == nil || *got.score != 1 {
			t.Fatalf("intent/source/score = %+v, want %q/%q/1", got, intent, source)
		}
		t.Logf("persisted launch values: owner=%q intent=%q source=%q score=1", owner, intent, source)
	}
	invoke := func(wantCode int, args ...string) string {
		t.Helper()
		out, err := h.Run(args...)
		code := 0
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("command failed without exit status: %v\n%s", err, out)
			}
			code = exit.ExitCode()
		}
		t.Logf("%v: exit=%d\n%s", args, code, out)
		if code != wantCode {
			t.Fatalf("exit=%d, want %d", code, wantCode)
		}
		if wantCode == 1 && (!strings.Contains(out, "run: queued") || !strings.Contains(out, "wait of 1s elapsed")) {
			t.Fatalf("expected a bounded wait on the queued run: %s", out)
		}
		return out
	}

	// Supplying neither owner nor intent must leave the hook-created row alone.
	invoke(1, "axi", "run", "--wait", "1s")
	check("", "", "")
	invoke(1, "axi", "run", "--owner", "lane-a", "--wait", "1s")
	check("lane-a", "", "")
	out = invoke(2, "axi", "run", "--owner", "lane-b", "--intent", "Must not leak through conflict", "--wait", "1s")
	if !strings.Contains(out, "owner_conflict") || !strings.Contains(out, "launch ownership is never replaced") {
		t.Fatalf("missing ownership refusal: %s", out)
	}
	check("lane-a", "", "")

	// Exercise the author's --intent-file route, not just inline --intent.
	const intent = "Preserve the supplied goal\nincluding its second line"
	file := filepath.Join(t.TempDir(), "intent.txt")
	if err := os.WriteFile(file, []byte("  "+intent+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	invoke(1, "axi", "run", "--owner", "lane-a", "--intent-file", file, "--wait", "1s")
	check("lane-a", intent, "agent")
	out = invoke(1, "axi", "run", "--owner", "lane-a", "--intent", "Replacement forbidden", "--wait", "1s")
	if strings.Contains(out, "applied its missing") {
		t.Fatalf("equal owner/existing intent unexpectedly adopted: %s", out)
	}
	check("lane-a", intent, "agent")
}

// R1's independent oracle is the intent inferred by the running product from
// a disposable transcript. A later caller's goal cannot replace it or cause
// an ordinary reattachment to fail merely because the queue snapshot is stale.
func TestReattachKeepsStartedInferredIntent(t *testing.T) {
	t.Setenv("NO_MISTAKES_OWNER", "")
	scenario := filepath.Join(t.TempDir(), "inferred.yaml")
	content := "actions:\n  - match: \"transcript of a developer's recent conversation with a coding agent\"\n    structured:\n      summary: \"Inferred goal from the transcript\"\n" + strings.TrimPrefix(reattachScenario, "actions:\n")
	if err := os.WriteFile(scenario, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
	seedClaudeTranscript(t, h.HomeDir, h.WorkDir, "parkme.txt")
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	h.CommitChange("feature/inferred", "parkme.txt", "inferred\n", "inferred")
	h.PushToGate("feature/inferred")
	run := waitForStepStatus(t, h, "feature/inferred", types.StepReview, types.StepStatusAwaitingApproval, 2*time.Minute)
	before := readRunIntent(t, h.NMHome, run.ID)
	if before.summary == nil || *before.summary != "Inferred goal from the transcript" || before.source == nil || *before.source != "claude" {
		t.Fatalf("product did not infer fixture intent: %+v", before)
	}
	for _, args := range [][]string{
		{"axi", "run", "--wait", "5s"},
		{"axi", "run", "--intent", "Caller goal must not replace inferred goal", "--wait", "5s"},
	} {
		out, err := h.Run(args...)
		t.Logf("%v: err=%v\n%s", args, err, out)
		if err != nil {
			t.Fatalf("ordinary reattach refused: %v", err)
		}
	}
	out, err := h.Run("axi", "run", "--owner", "lane-a", "--intent", "Caller goal", "--wait", "5s")
	t.Logf("late owner: err=%v\n%s", err, out)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 || !strings.Contains(out, "refusing to reattach (run_started)") || !strings.Contains(out, "no-mistakes axi abort --run "+run.ID) {
		t.Fatalf("late owner must exit 2 with the abort remedy: %v\n%s", err, out)
	}
	after := readRunIntent(t, h.NMHome, run.ID)
	if h.RunInfo(run.ID).Owner != "" || after.summary == nil || *after.summary != *before.summary || after.source == nil || *after.source != *before.source {
		t.Fatalf("reattach changed the persisted launch values: %+v", after)
	}
	t.Logf("persisted intent remained %q (source %q), owner remained empty", *after.summary, *after.source)
}
