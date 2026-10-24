package steps

import (
	"testing"
	"time"
)

// Issue #226: only a healthy, settled poll has an observation, and its
// outcome is part of it, so a readiness flip at identical forge reads resets
// the backoff instead of counting as unchanged.
func TestCIPollObservation(t *testing.T) {
	const forge = "state=OPEN base=main mergeable=MERGEABLE checks=build|pass"
	if ciPollObservation(forge, false, "CI checks running", false) != "" {
		t.Fatal("an unhealthy or unsettled poll must never count as unchanged")
	}
	if ciPollObservation("", true, "CI checks running", false) != "" {
		t.Fatal("a poll that read nothing must never count as unchanged")
	}
	notReady := ciPollObservation(forge, true, "review bot review is pending, waiting for completion...", false)
	readyNow := ciPollObservation(forge, true, "review bot review is pending, waiting for completion...", true)
	if notReady == "" || notReady == readyNow {
		t.Fatalf("a readiness flip at identical forge reads must change the observation: %q vs %q", notReady, readyNow)
	}
	if ciPollObservation(forge, true, "CI checks running", false) == ciPollObservation(forge, true, ciChecksPassedMsg, false) {
		t.Fatal("a status change at identical forge reads must change the observation")
	}
	if notReady != ciPollObservation(forge, true, "review bot review is pending, waiting for completion...", false) {
		t.Fatal("a healthy bot-pending poll that saw the same thing must stay unchanged, so it still backs off")
	}
}

func TestUnchangedPollMaxTestKnob(t *testing.T) {
	t.Setenv(ciUnchangedPollMaxEnv, "")
	if got := unchangedPollInterval(30*time.Second, 6); got != ciUnchangedPollMax {
		t.Fatalf("default backoff = %v, want the %v production cap", got, ciUnchangedPollMax)
	}
	for _, invalid := range []string{"soon", "0", "-1m"} {
		t.Setenv(ciUnchangedPollMaxEnv, invalid)
		if got := unchangedPollInterval(30*time.Second, 6); got != ciUnchangedPollMax {
			t.Fatalf("%q: backoff = %v, want the production default", invalid, got)
		}
	}
	t.Setenv(ciUnchangedPollMaxEnv, "30s")
	for _, interval := range []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second} {
		if got := unchangedPollInterval(interval, 6); got != interval {
			t.Fatalf("knob 30s: %v backed off to %v, want the elapsed-time schedule unchanged", interval, got)
		}
	}
}
