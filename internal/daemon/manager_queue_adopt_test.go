package daemon

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

// A post-receive push creates an ownerless, intent-less run. A later
// `axi run --owner X --intent ...` reattaches to it, and used to keep the
// empty owner and intent silently (issue #190). While that run is still
// queued and has never started, the daemon adopts the caller's owner and
// intent; once it has started, or when it already has a different owner, the
// adoption is refused so the caller can abort and relaunch.

func (f *schedFixture) adopt(runID, owner, intent string) ipc.AdoptRunLaunchResult {
	f.t.Helper()
	var result ipc.AdoptRunLaunchResult
	if err := f.client.Call(ipc.MethodAdoptRunLaunch, &ipc.AdoptRunLaunchParams{RunID: runID, Owner: owner, Intent: intent}, &result); err != nil {
		f.t.Fatalf("adopt run launch %s: %v", runID, err)
	}
	return result
}

func (f *schedFixture) launchSeen(runID string) [2]string {
	f.step.mu.Lock()
	defer f.step.mu.Unlock()
	return f.step.launch[runID]
}

func TestAdoptRunLaunchAppliesOwnerAndIntentToQueuedPushRun(t *testing.T) {
	f := newSchedFixture(t, "daemon:\n  max_concurrent_runs: 1\n")
	running, _ := f.push("a0")
	f.waitStarted(running)
	queued, _ := f.push("a1")
	f.assertQueued(queued, 1, 1)
	if got := f.info(queued).Owner; got != "" {
		t.Fatalf("push-created run owner = %q, want ownerless", got)
	}

	result := f.adopt(queued, "lane-a", "Ship the accounted head fix")
	if result.Refused != "" || !result.OwnerApplied || !result.IntentApplied {
		t.Fatalf("adopt queued run = %+v, want owner and intent applied", result)
	}
	if got := f.info(queued).Owner; got != "lane-a" {
		t.Fatalf("owner after adopt = %q, want lane-a", got)
	}
	stored, err := f.d.GetRun(queued)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Intent == nil || *stored.Intent != "Ship the accounted head fix" || stored.IntentSource == nil || *stored.IntentSource != db.RunIntentSourceAgent {
		t.Fatalf("stored intent = %v source %v, want the adopted authoritative intent", stored.Intent, stored.IntentSource)
	}

	// The same owner again is a no-op, and a second intent never replaces the
	// adopted one.
	again := f.adopt(queued, "lane-a", "a different goal")
	if again.Refused != "" || again.OwnerApplied || again.IntentApplied {
		t.Fatalf("repeat adopt = %+v, want a no-op", again)
	}

	f.step.release(running)
	f.waitStarted(queued)
	if seen := f.launchSeen(queued); seen != [2]string{"lane-a", "Ship the accounted head fix"} {
		t.Fatalf("queued run started with owner/intent %q, want the adopted ones", seen)
	}
	f.step.release(queued)
}

func TestAdoptRunLaunchRefusesStartedRun(t *testing.T) {
	f := newSchedFixture(t, "daemon:\n  max_concurrent_runs: 1\n")
	running, _ := f.push("s0")
	f.waitStarted(running)

	result := f.adopt(running, "lane-a", "Ship it")
	if result.Refused != ipc.AdoptRunLaunchRefusedStarted || result.OwnerApplied || result.IntentApplied || result.Message == "" {
		t.Fatalf("adopt started run = %+v, want a run_started refusal with a message", result)
	}
	if got := f.info(running).Owner; got != "" {
		t.Fatalf("started run owner = %q after a refused adopt, want unchanged", got)
	}
	// Nothing to adopt (no owner, no intent) is never a refusal.
	if none := f.adopt(running, "", ""); none.Refused != "" {
		t.Fatalf("empty adopt = %+v, want no refusal", none)
	}
	f.step.release(running)
}

func TestAdoptRunLaunchRefusesADifferentOwner(t *testing.T) {
	f := newSchedFixture(t, "daemon:\n  max_concurrent_runs: 1\n")
	running, _ := f.push("c0")
	f.waitStarted(running)
	queued, _ := f.push("c1")
	if first := f.adopt(queued, "lane-a", ""); !first.OwnerApplied {
		t.Fatalf("first adopt = %+v, want owner applied", first)
	}
	result := f.adopt(queued, "lane-b", "")
	if result.Refused != ipc.AdoptRunLaunchRefusedOwnerConflict || result.OwnerApplied || result.Message == "" {
		t.Fatalf("adopt with a different owner = %+v, want an owner_conflict refusal", result)
	}
	if got := f.info(queued).Owner; got != "lane-a" {
		t.Fatalf("owner after conflicting adopt = %q, want lane-a kept", got)
	}
	f.step.release(running)
	f.waitStarted(queued)
	f.step.release(queued)
}
