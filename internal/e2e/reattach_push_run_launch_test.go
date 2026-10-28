//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A plain `git push` to the gate creates a run with no owner and no intent.
// `axi run --owner --intent` that then reattaches to it used to keep both empty
// without a word (issue #190). Through the real binary, hook and daemon: a
// still-queued push run takes the caller's owner and intent and starts with
// them; a push run that has already started refuses the reattach with the
// abort-and-relaunch remedy and stays ownerless.

// reattachScenario parks any review whose only changed file is parkme.txt and
// is clean everywhere else.
const reattachScenario = `actions:
  - match: "minus ignored paths):\n- parkme.txt"
    structured:
      findings:
        - id: hold
          severity: warning
          description: Hold this run at the review gate
          action: ask-user
      summary: "hold"
      risk_level: low
      risk_rationale: "fixture"
      risk_scope: source-or-external
  - structured:
      findings: []
      summary: "clean"
      risk_level: low
      risk_rationale: "none"
      risk_scope: source-or-external
      tested: ["fixture"]
      testing_summary: "fixture"
      scenarios:
        - name: "fixture"
          result: pass
          live: true
          evidence: "fixture"
          reason: ""
      verdict: go
      artifacts: []
      title: "feat: fixture"
      body: "Fixture"
`

func newReattachHarness(t *testing.T, globalExtra string) *Harness {
	t.Helper()
	scenario := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(scenario, []byte(reattachScenario), 0600); err != nil {
		t.Fatal(err)
	}
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario, GlobalConfigExtra: globalExtra})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	return h
}

func TestReattachAdoptsOwnerAndIntentForQueuedPushRun(t *testing.T) {
	h := newReattachHarness(t, "daemon:\n  max_concurrent_runs: 1\n  hold_slot_while_parked: true\n")

	// Hold the only slot with a run parked at its review gate.
	h.CommitChange("blocker", "parkme.txt", "hold\n", "blocker")
	if out, err := h.Run("axi", "run", "--intent", "Hold the slot", "--wait", "2m"); err != nil && !strings.Contains(out, "awaiting") {
		t.Fatalf("blocker run: %v\n%s", err, out)
	}
	blocker := waitForStepStatus(t, h, "blocker", types.StepReview, types.StepStatusAwaitingApproval, 2*time.Minute)

	h.CommitChange("feature/pushed", "adopt.txt", "adopt\n", "pushed")
	h.PushToGate("feature/pushed")
	pushed := h.ActiveRun("feature/pushed")
	if pushed == nil || pushed.Status != types.RunQueued || pushed.Owner != "" {
		t.Fatalf("push-created run = %+v, want an ownerless queued run", pushed)
	}

	const intent = "Ship the pushed adoption change"
	out, err := h.Run("axi", "run", "--owner", "lane-a", "--intent", intent, "--wait", "5s")
	t.Logf("reattach: err=%v\n%s", err, out)
	if !strings.Contains(out, "applied its missing owner lane-a and intent") {
		t.Fatalf("reattach did not report adopting owner and intent:\n%s", out)
	}
	if got := h.RunInfo(pushed.ID).Owner; got != "lane-a" {
		t.Fatalf("owner after reattach = %q, want lane-a", got)
	}

	// Free the slot; the pushed run must start with the adopted intent.
	h.Respond(blocker.ID, types.StepReview, types.ActionApprove)
	deadline := time.Now().Add(3 * time.Minute)
	for {
		for _, inv := range h.AgentInvocations() {
			if strings.Contains(inv.Prompt, "- adopt.txt") && strings.Contains(inv.Prompt, intent) {
				t.Logf("pushed run %s reviewed with the adopted intent", pushed.ID)
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("pushed run %s never reached a review prompt carrying the adopted intent", pushed.ID)
		}
		time.Sleep(time.Second)
	}
}

func TestReattachRefusesStartedOwnerlessPushRun(t *testing.T) {
	h := newReattachHarness(t, "")

	h.CommitChange("feature/started", "parkme.txt", "started\n", "started")
	h.PushToGate("feature/started")
	run := waitForStepStatus(t, h, "feature/started", types.StepReview, types.StepStatusAwaitingApproval, 2*time.Minute)

	out, err := h.Run("axi", "run", "--owner", "lane-a", "--intent", "Too late", "--wait", "5s")
	if err == nil {
		t.Fatalf("reattach to a started ownerless push run succeeded:\n%s", out)
	}
	for _, want := range []string{"refusing to reattach (run_started)", "no-mistakes axi abort --run " + run.ID} {
		if !strings.Contains(out, want) {
			t.Fatalf("refusal missing %q:\n%s", want, out)
		}
	}
	if got := h.RunInfo(run.ID).Owner; got != "" {
		t.Fatalf("refused reattach changed the owner to %q", got)
	}
	t.Logf("started push run %s refused the reattach and stayed ownerless", run.ID)
}
