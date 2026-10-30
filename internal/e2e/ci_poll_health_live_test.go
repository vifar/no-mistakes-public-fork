//go:build e2e && !windows

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCITrustedNoCIBackoffLiveJourney(t *testing.T) {
	t.Setenv("NM_TEST_CI_UNCHANGED_POLL_MAX", "")
	j := newScopeJourney(t, SetupOpts{Agent: "claude"})
	j.setTrusted("no_ci: true\n")
	j.setForge(forgeState{Mergeable: "UNKNOWN"})
	j.init()
	const branch = "feature/trusted-no-ci-backoff"
	j.push(branch)
	run := j.expectWaiting(branch, "mergeable state still pending:")
	start := len(liveCIPollTimes(t, j)) - 1
	polls := waitLiveCIPolls(t, j, start+3, 110*time.Second)
	assertLiveCIWaits(t, "trusted no_ci exempts absent checks from registration waits", polls[start:start+3], []time.Duration{30 * time.Second, 60 * time.Second})
	if j.h.RunInfo(run.ID).CIReady {
		t.Fatal("unresolved mergeability became ready under no_ci")
	}
	j.finish(branch)
}

// A failed Git fetch does not necessarily log a warning. Keep forge reads
// healthy while denying only fetch at the external process boundary, then
// exercise both the timeout's base read and BEHIND's separate base read.
func TestCIBaseTipOutageCadenceLiveJourney(t *testing.T) {
	t.Setenv("NM_TEST_CI_UNCHANGED_POLL_MAX", "")
	j := newScopeJourney(t, SetupOpts{Agent: "claude"})
	j.setTrusted("ci:\n  rebase_behind: true\n")
	j.setForge(forgeState{Checks: []forgeCheck{runningCheck("build")}})
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	deny := filepath.Join(filepath.Dir(j.h.AgentLog), "deny-fetch")
	wrapper := "#!/bin/sh\nfor arg in \"$@\"; do\n" +
		"  if [ \"$arg\" = fetch ] && [ -f " + shellQuote(deny) + " ]; then\n" +
		"    printf '%s\\n' 'disposable transport read denied' >&2\n    exit 128\n  fi\ndone\n" +
		"exec " + shellQuote(realGit) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(j.h.BinDir, "git"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	j.init()
	const branch = "feature/base-tip-outage-cadence"
	j.push(branch)
	run := j.expectWaiting(branch, "CI checks running")
	start := len(liveCIPollTimes(t, j))
	if err := os.WriteFile(deny, []byte("deny only fetch, preserve forge head reads\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	polls := waitLiveCIPolls(t, j, start+3, 150*time.Second)
	assertLiveCIWaits(t, "pending checks with a failed timeout base-tip fetch", polls[start:start+3], []time.Duration{30 * time.Second, 30 * time.Second})
	j.setForge(forgeState{Checks: []forgeCheck{greenCheck("build")}, MergeStateStatus: "BEHIND"})
	// These transitions can include the rest of the current poll, the next
	// wait, and the next poll's reads. Their observation budget must exceed
	// a single accepted poll gap; assertLiveCIWaits still enforces cadence.
	j.waitCILog(run.ID, "waiting for the base to settle before rebasing", scopeCheckWaitTime)
	start = len(liveCIPollTimes(t, j)) - 1
	polls = waitLiveCIPolls(t, j, start+3, 110*time.Second)
	assertLiveCIWaits(t, "green BEHIND checks with a failed base-tip fetch", polls[start:start+3], []time.Duration{30 * time.Second, 30 * time.Second})
	if j.h.RunInfo(run.ID).CIReady || len(j.ciRepairPrompts()) != 0 {
		t.Fatal("unresolved base-tip proof became ready or started a repair")
	}
	j.setForge(forgeState{Checks: []forgeCheck{greenCheck("build")}})
	j.waitCILog(run.ID, "warning: CI readiness comparison could not be verified:", scopeCheckWaitTime)
	start = len(liveCIPollTimes(t, j)) - 1
	polls = waitLiveCIPolls(t, j, start+3, 110*time.Second)
	assertLiveCIWaits(t, "green checks with a failed comparison fetch", polls[start:start+3], []time.Duration{30 * time.Second, 30 * time.Second})
	if j.h.RunInfo(run.ID).CIReady {
		t.Fatal("failed live comparison became ready")
	}
	if err := os.Remove(deny); err != nil {
		t.Fatal(err)
	}
	j.setForge(forgeState{Checks: []forgeCheck{greenCheck("build")}})
	j.expectReady(branch, scopeCheckWaitTime)
	t.Logf("Live base-tip outage monitor transcript:\n%s", j.ciLog(run.ID))
	j.finish(branch)
}

// Issue #226 is a wall-clock contract: registration waits and failed reads
// must retain the normal cadence, while healthy review-bot waits still back
// off. Exercise the real daemon without either polling override. The forge
// invocation timestamps are boundary observations, not a substituted clock.
func TestCIPollHealthCadenceLiveJourney(t *testing.T) {
	t.Setenv("NM_TEST_CI_UNCHANGED_POLL_MAX", "")
	for _, required := range []bool{false, true} {
		name := "empty-rollup"
		if required {
			name = "missing-required-check"
		}
		t.Run(name, func(t *testing.T) {
			j := newScopeJourney(t, SetupOpts{Agent: "claude"})
			trusted := "ci:\n  review_bot_comments: always\n"
			if required {
				trusted += "  must_pass: [build]\n"
			}
			j.setTrusted(trusted)
			j.setForge(forgeState{Mergeable: "UNKNOWN"})
			j.init()
			branch := "feature/poll-health-" + name
			j.push(branch)
			run := j.expectWaiting(branch, "mergeable state still pending:")

			polls := liveCIPollTimes(t, j)
			start := len(polls) - 1
			polls = waitLiveCIPolls(t, j, start+4, 110*time.Second)
			assertLiveCIWaits(t, "unregistered UNKNOWN head", polls[start:start+4], []time.Duration{30 * time.Second, 30 * time.Second, 30 * time.Second})
			if j.h.RunInfo(run.ID).CIReady {
				t.Fatal("unregistered head became ready")
			}
			if !required {
				j.finish(branch)
				return
			}

			// Leave the checks identical across pending, ready, read failure
			// and recovery. Only the bot read and monitor outcome change.
			forge := forgeState{Checks: []forgeCheck{greenCheck("build")}}
			forge.ReviewSummaries = []json.RawMessage{json.RawMessage(`{"id":7,"body":"@codex review","created_at":"2026-10-21T12:00:00Z"}`)}
			forge.ReviewReactions = map[string][]json.RawMessage{"7": {json.RawMessage(`{"user":{"login":"chatgpt-codex-connector[bot]"},"content":"eyes"}`)}}
			j.setForge(forge)
			j.waitCILog(run.ID, "review bot review is pending", 45*time.Second)
			start = len(liveCIPollTimes(t, j)) - 1
			polls = waitLiveCIPolls(t, j, start+3, 110*time.Second)
			assertLiveCIWaits(t, "healthy bot pending", polls[start:start+3], []time.Duration{30 * time.Second, 60 * time.Second})
			if j.h.RunInfo(run.ID).CIReady || len(j.ciRepairPrompts()) != 0 {
				t.Fatal("healthy pending review became ready or spent a repair round")
			}

			forge.ReviewReactions["7"] = append(forge.ReviewReactions["7"], json.RawMessage(`{"user":{"login":"chatgpt-codex-connector[bot]"},"content":"+1"}`))
			j.setForge(forge)
			j.expectReady(branch, 145*time.Second)
			start = len(liveCIPollTimes(t, j)) - 1
			polls = waitLiveCIPolls(t, j, start+2, 75*time.Second)
			assertLiveCIWaits(t, "healthy readiness flip at identical check rollup", polls[start:start+2], []time.Duration{60 * time.Second})

			forge.ReviewError = &forgeEndpoint{Status: 403, Message: "disposable review read denied"}
			j.setForge(forge)
			j.waitCILog(run.ID, "warning: could not read review bot comments:", 145*time.Second)
			start = len(liveCIPollTimes(t, j)) - 1
			polls = waitLiveCIPolls(t, j, start+3, 150*time.Second)
			assertLiveCIWaits(t, "review read failure after five minutes", polls[start:start+3], []time.Duration{60 * time.Second, 60 * time.Second})
			if j.h.RunInfo(run.ID).CIReady {
				t.Fatal("failed review read preserved readiness")
			}

			forge.ReviewError = nil
			j.setForge(forge)
			j.expectReady(branch, 75*time.Second)
			start = len(liveCIPollTimes(t, j)) - 1
			polls = waitLiveCIPolls(t, j, start+2, 75*time.Second)
			assertLiveCIWaits(t, "recovered readiness at identical check rollup", polls[start:start+2], []time.Duration{60 * time.Second})
			out, err := j.h.Run("axi", "status", "--run", run.ID)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("Recovered CLI status:\n%s\nMonitor transcript:\n%s", out, j.ciLog(run.ID))
			j.finish(branch)
		})
	}
}

func liveCIPollTimes(t *testing.T, j *scopeJourney) []time.Time {
	t.Helper()
	raw, err := os.ReadFile(j.ghLogPath)
	if err != nil {
		t.Fatal(err)
	}
	var times []time.Time
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var call struct {
			Time time.Time `json:"time"`
			Args []string  `json:"args"`
		}
		if err := json.Unmarshal([]byte(line), &call); err != nil {
			t.Fatal(err)
		}
		if len(call.Args) < 2 || call.Args[0] != "pr" || call.Args[1] != "view" {
			continue
		}
		for _, arg := range call.Args {
			fields := strings.Split(arg, ",")
			seen := map[string]bool{}
			for _, field := range fields {
				seen[field] = true
			}
			if seen["state"] && seen["mergeable"] && seen["headRefOid"] {
				times = append(times, call.Time)
				break
			}
		}
	}
	return times
}

func waitLiveCIPolls(t *testing.T, j *scopeJourney, count int, timeout time.Duration) []time.Time {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		polls := liveCIPollTimes(t, j)
		if len(polls) >= count {
			// The combined view opens a poll. Let its closing reads finish
			// before the test changes the disposable forge's next response.
			time.Sleep(time.Second)
			return polls
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("wanted %d live CI polls, got %d", count, len(liveCIPollTimes(t, j)))
	return nil
}

func assertLiveCIWaits(t *testing.T, phase string, polls []time.Time, want []time.Duration) {
	t.Helper()
	for i, expected := range want {
		got := polls[i+1].Sub(polls[i])
		t.Logf("%s: %s -> %s, elapsed=%v, expected cadence=%v", phase, polls[i].Format(time.RFC3339Nano), polls[i+1].Format(time.RFC3339Nano), got, expected)
		// The public 30/60s cadence is the oracle; allow process startup
		// overhead, while rejecting the former 60/120s unhealthy backoff.
		if got < expected-2*time.Second || got > expected+15*time.Second {
			t.Fatalf("%s: poll gap %v, want %v with scheduling tolerance", phase, got, expected)
		}
	}
}
