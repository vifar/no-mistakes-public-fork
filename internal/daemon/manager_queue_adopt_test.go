package daemon

import (
	"strings"
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

// Once a queued run starts, launchQueuedRun executes a freshly loaded record
// and the intent step may persist an inferred intent; the m.runs snapshot the
// queue held keeps its original empty values. Adoption must decide from the
// durable row, so an ordinary reattach that supplies --intent to that run is
// not refused, while an owner it would still lose is.
func TestAdoptRunLaunchKeepsOrdinaryReattachToStartedRunWithPersistedIntent(t *testing.T) {
	f := newSchedFixture(t, "daemon:\n  max_concurrent_runs: 1\n")
	running, _ := f.push("i0")
	f.waitStarted(running)
	if err := f.d.UpdateRunIntent(running, db.RunIntent{Summary: "inferred goal", Source: "claude", Score: 0.5}); err != nil {
		t.Fatal(err)
	}

	result := f.adopt(running, "", "caller goal")
	if result.Refused != "" || result.OwnerApplied || result.IntentApplied {
		t.Fatalf("reattach with intent to a started run that has one = %+v, want a no-op", result)
	}
	stored, err := f.d.GetRun(running)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Intent == nil || *stored.Intent != "inferred goal" {
		t.Fatalf("stored intent = %v, want the persisted inferred intent kept", stored.Intent)
	}
	if owned := f.adopt(running, "lane-a", "caller goal"); owned.Refused != ipc.AdoptRunLaunchRefusedStarted {
		t.Fatalf("owner on a started ownerless run = %+v, want run_started", owned)
	}
	f.step.release(running)
}

// A launch receipt binds its original owner, including an empty owner.
// Ordinary reattachment must not break an otherwise valid nonce replay.
func TestAdoptRunLaunchPreservesNonceBoundOwner(t *testing.T) {
	f := newSchedFixture(t, "daemon:\n  max_concurrent_runs: 1\n")
	running, _ := f.push("nonce-blocker")
	f.waitStarted(running)
	params := &ipc.StartFreshRunParams{RepoID: f.repoID, Branch: "main", HeadSHA: f.head, Intent: "original goal", LaunchNonce: "bound-owner", ValidationGeneration: "generation-1"}
	var first ipc.StartFreshRunResult
	if err := f.client.Call(ipc.MethodStartFreshRun, params, &first); err != nil {
		t.Fatal(err)
	}
	result := f.adopt(first.Receipt.RunID, "lane-a", "replacement goal")
	stored, err := f.d.GetRun(first.Receipt.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Refused == "" || result.OwnerApplied || result.IntentApplied || stored.Owner() != "" || stored.Intent == nil || *stored.Intent != "original goal" {
		t.Fatalf("nonce adoption = %+v, stored owner=%q intent=%v", result, stored.Owner(), stored.Intent)
	}
	var replay ipc.StartFreshRunResult
	if err := f.client.Call(ipc.MethodStartFreshRun, params, &replay); err != nil {
		t.Fatalf("original launch no longer replays: %v", err)
	}
	if replay.Receipt.RunID != first.Receipt.RunID || replay.Receipt.Disposition != "reused" {
		t.Fatalf("replay=%+v first=%+v", replay, first)
	}
}

func TestAdoptRunLaunchRejectsInvalidOwnerBeforeMutation(t *testing.T) {
	f := newSchedFixture(t, "daemon:\n  max_concurrent_runs: 1\n")
	running, _ := f.push("invalid-owner-blocker")
	f.waitStarted(running)
	queued, _ := f.push("invalid-owner-queued")
	for _, owner := range []string{strings.Repeat("x", 257), "lane\x00a", "lane\na", "lane-a\n"} {
		var result ipc.AdoptRunLaunchResult
		err := f.client.Call(ipc.MethodAdoptRunLaunch, &ipc.AdoptRunLaunchParams{RunID: queued, Owner: owner, Intent: "must not persist"}, &result)
		stored, readErr := f.d.GetRun(queued)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if err == nil || !strings.Contains(err.Error(), "owner must be valid UTF-8") {
			t.Errorf("owner %q: error=%v; expected owner validation refusal", owner, err)
		}
		if stored.Owner() != "" || stored.Intent != nil {
			t.Fatalf("rejected owner %q mutated owner=%q intent=%v", owner, stored.Owner(), stored.Intent)
		}
	}
}
