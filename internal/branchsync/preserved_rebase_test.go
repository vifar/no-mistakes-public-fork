package branchsync

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Real independent repositories: P exists only in the gate, R precedes L,
// and the pipeline rebased onto a newer base before adding fixes.
func newPreservedRebaseFixture(t *testing.T) (*recoverFixture, string) {
	t.Helper()
	f := newRecoverFixture(t, types.RunCancelled)
	pipeline := filepath.Join(filepath.Dir(f.local), "pipeline")
	mustRun(t, pipeline, "checkout", "-b", "new-base", f.base)
	mustWrite(t, filepath.Join(pipeline, "base-only.txt"), "new base\n")
	mustRun(t, pipeline, "add", ".")
	mustRun(t, pipeline, "commit", "-m", "advance base")
	base := mustRun(t, pipeline, "rev-parse", "HEAD")
	mustRun(t, pipeline, "checkout", "feature/recover")
	mustRun(t, pipeline, "rebase", "--onto", base, f.base)
	f.preserved = mustRun(t, pipeline, "rev-parse", "HEAD")
	mustRun(t, pipeline, "push", "--force", "origin", "HEAD:refs/heads/feature/recover")
	mustRun(t, f.gate, "update-ref", f.anchorRef(), f.preserved)
	// The gate branch is still R, while P is pinned separately.
	mustRun(t, f.gate, "update-ref", "refs/heads/feature/recover", f.submitted)
	mustRun(t, f.local, "push", f.remote, "HEAD:refs/heads/feature/recover")
	if err := f.db.UpdateRunPushBinding(f.run.ID, db.PushBinding{HeadSHA: f.submitted, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(f.remote), Ref: "refs/heads/feature/recover"}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunHeadSHA(f.run.ID, f.preserved); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunStatusWithVerifiedHead(f.run.ID, types.RunCancelled, f.preserved); err != nil {
		t.Fatal(err)
	}
	f.run, _ = f.db.GetRun(f.run.ID)
	mustWrite(t, filepath.Join(f.local, "local-only.txt"), "operator work\n")
	mustRun(t, f.local, "add", ".")
	mustRun(t, f.local, "commit", "-m", "local follow-up")
	local := mustRun(t, f.local, "rev-parse", "HEAD")
	if objectExists(f.ctx, f.gate, local) || objectExists(f.ctx, f.local, f.preserved) {
		t.Fatal("fixture histories leaked between repositories")
	}
	t.Logf("R=%s L=%s P=%s ref=%s", f.submitted, local, f.preserved, f.anchorRef())
	return f, local
}

func TestPreservedRebaseOffersExecutableKeepLocal(t *testing.T) {
	f, local := newPreservedRebaseFixture(t)
	for _, state := range []State{f.service.InspectCached(f.ctx), f.service.Refresh(f.ctx)} {
		t.Logf("classification: safety=%s next=%+v note=%s", state.Safety, state.NextAction, state.Error)
		want := "no-mistakes axi sync --recover --keep-local --authoritative-head " + local
		if state.NextAction == nil || state.NextAction.Code != "recover_custody" || state.NextAction.Command != want {
			t.Fatalf("want %s, got %+v", want, state.NextAction)
		}
		if !strings.Contains(state.Error, f.preserved) || !strings.Contains(state.Error, f.anchorRef()) {
			t.Fatalf("missing preservation disclosure: %s", state.Error)
		}
	}
	refused := f.service.Recover(f.ctx, false)
	if refused.Recovered || f.custodyReturned() {
		t.Fatal("plain recovery took custody")
	}
	wrong := f.service.RecoverAtHead(f.ctx, true, f.submitted)
	if wrong.Recovered || f.custodyReturned() {
		t.Fatal("wrong authoritative head accepted")
	}
	recovered := f.service.RecoverAtHead(f.ctx, true, local)
	if !recovered.Recovered || !f.custodyReturned() {
		t.Fatalf("offered recovery failed: %+v", recovered)
	}
	for _, dir := range []string{f.local, f.gate} {
		if got := mustRun(t, dir, "rev-parse", f.anchorRef()); got != f.preserved {
			t.Fatalf("lost P at %s", dir)
		}
		if got := mustRun(t, dir, "rev-parse", "refs/heads/feature/recover"); got != local {
			t.Fatalf("lost L at %s", dir)
		}
	}
	t.Logf("recovered: L=%s in worktree/gate; P=%s anchored in both; custody returned", local, f.preserved)
}

func TestPreservedRebaseOfferFailsClosed(t *testing.T) {
	for _, fault := range []string{"missing", "mismatch", "dirty", "symbolic"} {
		t.Run(fault, func(t *testing.T) {
			f, local := newPreservedRebaseFixture(t)
			switch fault {
			case "missing":
				mustRun(t, f.gate, "update-ref", "-d", f.anchorRef())
			case "mismatch":
				mustRun(t, f.gate, "update-ref", f.anchorRef(), f.submitted)
			case "symbolic":
				mustRun(t, f.gate, "symbolic-ref", f.anchorRef(), "refs/heads/feature/recover")
			case "dirty":
				mustWrite(t, filepath.Join(f.local, "dirty.txt"), "dirty\n")
			}
			state := f.service.InspectCached(f.ctx)
			if state.NextAction != nil && strings.Contains(state.NextAction.Command, "--keep-local") {
				t.Fatal(fmt.Sprintf("unsafe offer: %+v", state))
			}
			recovered := f.service.RecoverAtHead(f.ctx, true, local)
			if recovered.Recovered {
				t.Fatalf("unsafe authoritative recovery: %+v", recovered)
			}
			if f.custodyReturned() {
				t.Fatal("inspection returned custody")
			}
		})
	}
}

func TestPreservedRebaseForeignRemoteRefuses(t *testing.T) {
	f, local := newPreservedRebaseFixture(t)
	mustRun(t, f.local, "push", f.remote, "HEAD:refs/heads/feature/recover")
	for _, state := range []State{f.service.Refresh(f.ctx), f.service.RecoverAtHead(f.ctx, true, local)} {
		if state.Recovered || state.Safety != "blocked_recover_remote_changed" {
			t.Fatalf("foreign remote accepted: %+v", state)
		}
	}
	if f.custodyReturned() {
		t.Fatal("foreign remote returned custody")
	}
}

func TestPreservedRebaseKeepLocalGateRaceRefuses(t *testing.T) {
	f, local := newPreservedRebaseFixture(t)
	f.service.beforeGateReset = func() { mustRun(t, f.gate, "update-ref", "refs/heads/feature/recover", f.preserved) }
	state := f.service.RecoverAtHead(f.ctx, true, local)
	if state.Recovered || f.custodyReturned() {
		t.Fatalf("gate race accepted: %+v", state)
	}
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != f.preserved {
		t.Fatal("clobbered concurrent gate change")
	}
}
