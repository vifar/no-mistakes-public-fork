//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A clean review whose reviewed_paths covers only part of the change must
// converge inside ONE round: each focused completion pass is sent only the
// files still unverified and the round keeps what earlier passes reported.
// A pass that verifies nothing new parks with the remaining files named and
// never credits a file the reviewer did not report.
func TestReviewCoverageConvergesWithinOneRound(t *testing.T) {
	// Each completion prompt lists the still-unverified files as indented
	// "  - path" lines after this header, so the first listed path tells the
	// fake reviewer which pass it is answering.
	const missingHeader = "The files still unverified are:\\n  - "
	for _, tc := range []struct {
		name          string
		secondPass    string
		converges     bool
		wantLogPieces []string
	}{
		{
			name:       "partial passes converge",
			secondPass: "[gamma.txt]",
			converges:  true,
			wantLogPieces: []string{
				"running one focused review pass over 2 unverified file(s): beta.txt, gamma.txt",
				"running one focused review pass over 1 unverified file(s): gamma.txt",
			},
		},
		{
			name:       "no progress parks",
			secondPass: "[beta.txt]",
			wantLogPieces: []string{
				"focused coverage pass made no progress",
				"1 reviewable file(s) unverified: gamma.txt",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scenario := filepath.Join(t.TempDir(), "agent.yaml")
			body := fmt.Sprintf(`actions:
  - match: "%[1]sbeta.txt"
    structured:
      findings: []
      summary: "first completion pass"
      risk_level: low
      risk_rationale: "checked beta"
      risk_scope: source-or-external
      reviewed_paths: [beta.txt]
  - match: "%[1]sgamma.txt"
    structured:
      findings: []
      summary: "second completion pass"
      risk_level: low
      risk_rationale: "checked the remainder"
      risk_scope: source-or-external
      reviewed_paths: %[2]s
  - match: "Review the code changes and return structured findings"
    structured:
      findings: []
      summary: "partial first pass"
      risk_level: low
      risk_rationale: "checked alpha"
      risk_scope: source-or-external
      reviewed_paths: [alpha.txt]
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
      title: "feat: three files"
      body: "Three files"
`, missingHeader, tc.secondPass)
			if err := os.WriteFile(scenario, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
			if out, err := h.Run("init"); err != nil {
				t.Fatalf("init: %v\n%s", err, out)
			}
			branch := "feature/coverage-convergence"
			h.CommitChange(branch, "alpha.txt", "alpha\n", "alpha")
			h.CommitChange(branch, "beta.txt", "beta\n", "beta")
			h.CommitChange(branch, "gamma.txt", "gamma\n", "gamma")
			out, err := h.Run("axi", "run", "--intent", "Add three files")
			t.Logf("axi run: err=%v\n%s", err, out)

			var run *ipc.RunInfo
			if tc.converges {
				run = h.WaitForRun(branch, 3*time.Minute)
			} else {
				run = waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)
			}
			logs, err := h.Run("axi", "logs", "--step", "review", "--full")
			if err != nil {
				t.Fatalf("review logs: %v\n%s", err, logs)
			}
			for _, piece := range tc.wantLogPieces {
				if !strings.Contains(logs, piece) {
					t.Fatalf("review log missing %q:\n%s", piece, logs)
				}
			}
			if tc.converges {
				if run.Status != types.RunCompleted {
					t.Fatalf("one round of partial passes must certify the head: status=%s error=%v\n%s", run.Status, run.Error, logs)
				}
				if strings.Contains(logs, "unverified: ") && strings.Contains(logs, "parking") {
					t.Fatalf("converging round must not park:\n%s", logs)
				}
				t.Logf("run %s completed after one round of partial coverage passes", run.ID)
				return
			}
			status, err := h.Run("axi", "status")
			if err != nil || !strings.Contains(status, "awaiting_approval") {
				t.Fatalf("no-progress round must park at Review: %v\n%s", err, status)
			}
			t.Logf("run %s parked at Review with gamma.txt unverified", run.ID)
		})
	}
}
