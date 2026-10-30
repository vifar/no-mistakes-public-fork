package branchsync

import (
	"slices"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/custody"
)

// The issue #233 acceptance contract requires every unpublished local and
// pipeline commit to remain reachable from a tool-owned anchor after either
// exit. Git reachability from recovery refs is the independent oracle; leaving
// the kept local history only on mutable branch refs does not meet that contract.
func TestTerminalDivergenceKeepLocalAnchorsBothUnpublishedHistories(t *testing.T) {
	t.Parallel()
	for _, shape := range terminalDivergenceShapes {
		t.Run(shape.name, func(t *testing.T) {
			t.Parallel()
			f := newTerminalDivergenceFixture(t, shape)
			result := f.service.ExitTerminalDivergence(f.ctx, CustodyExitRequest{
				Mode: CustodyExitKeepLocal, Acknowledge: f.preserved, Invoker: "test-owner",
			})
			if !result.Recovered {
				t.Fatalf("acknowledged keep-local refused: %#v", result)
			}

			anchored := make(map[string]bool)
			for _, dir := range []string{f.local, f.gate} {
				anchorHeads := strings.Fields(mustRun(t, dir, "for-each-ref", "--format=%(objectname)", "refs/no-mistakes/"))
				if len(anchorHeads) == 0 {
					continue
				}
				args := append([]string{"rev-list"}, anchorHeads...)
				for _, sha := range strings.Fields(mustRun(t, dir, args...)) {
					anchored[sha] = true
				}
			}
			for _, head := range []string{f.submitted, f.preserved} {
				if !anchored[head] {
					t.Errorf("acknowledged keep-local returned custody without a tool-owned recovery anchor reaching unpublished head %s", head)
				}
			}
			exit := f.exitRecord()
			if exit == nil || !slices.Contains(exit.Anchors, custody.RecoveryLocalRef(f.run.ID)) {
				t.Fatalf("custody exit omits the local recovery anchor: %#v", exit)
			}
		})
	}
	t.Run("conflicting_local_anchor", func(t *testing.T) {
		t.Parallel()
		f := newTerminalDivergenceFixture(t, terminalDivergenceShapes[1])
		ref := custody.RecoveryLocalRef(f.run.ID)
		mustRun(t, f.local, "update-ref", ref, f.base)
		result := f.service.ExitTerminalDivergence(f.ctx, CustodyExitRequest{
			Mode: CustodyExitKeepLocal, Acknowledge: f.preserved, Invoker: "test-owner",
		})
		if result.Recovered || result.Safety != "blocked_recover_preserve_failed" {
			t.Fatalf("conflicting local anchor did not refuse the exit: %#v", result)
		}
		f.assertUntouched()
		if got := mustRun(t, f.local, "rev-parse", ref); got != f.base {
			t.Fatalf("conflicting local anchor changed to %s", got)
		}
	})
}
