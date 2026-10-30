package branchsync

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// terminalDivergenceShape is one of the three stranded custody states measured
// on 2026-10-29 (issue #233). In every one a terminal run published nothing,
// the clean registered worktree still sits at the run's submitted head,
// terminalization pinned the pipeline's conflict-resolved rebase at
// refs/no-mistakes/recover/<run> in the gate, and the gate branch still holds
// the submitted head. They differ only in what the recovery code observes
// around that core.
type terminalDivergenceShape struct {
	name   string
	status types.RunStatus
	// preservedInWorktree models the keeper line (controller#8291): an
	// operator backup ref already holds the preserved candidate locally, so the
	// relation reads diverged instead of unknown.
	preservedInWorktree bool
	// earlierPublished models a branch whose PR an EARLIER run published
	// (controller#8291 at c07c6d53, controller#9335 at 02f9d691): the remote
	// branch exists, but this run's own push binding is empty.
	earlierPublished bool
	wantRelation     string
}

var terminalDivergenceShapes = []terminalDivergenceShape{
	{name: "failed_review_preserved_in_worktree_8291", status: types.RunFailed, preservedInWorktree: true, earlierPublished: true, wantRelation: RelationDiverged},
	{name: "cancelled_lint_never_published_9028", status: types.RunCancelled, wantRelation: RelationUnknown},
	{name: "cancelled_test_earlier_pr_head_9335", status: types.RunCancelled, earlierPublished: true, wantRelation: RelationUnknown},
}

// terminalDivergenceFixture extends recoverFixture with the commits the
// acknowledgement lists must name.
type terminalDivergenceFixture struct {
	*recoverFixture
	// laneCommit is the operator's own commit whose content the pipeline's
	// conflict resolution rewrote, so no patch in the preserved head matches.
	laneCommit string
	// draftCommit is an operator commit the rebase replayed cleanly; it is set
	// only for earlierPublished shapes.
	draftCommit string
	// reviewFix is a pipeline commit that exists only in the preserved head.
	reviewFix string
}

func newTerminalDivergenceFixture(t *testing.T, shape terminalDivergenceShape) *terminalDivergenceFixture {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "upstream.git")
	mustRun(t, root, "init", "--bare", remote)

	local := filepath.Join(root, "operator")
	mustRun(t, root, "init", "-b", "main", local)
	configureIdentity(t, local)
	mustWrite(t, filepath.Join(local, "file.txt"), "base\nline two\n")
	mustRun(t, local, "add", "file.txt")
	mustRun(t, local, "commit", "-m", "base")
	base := mustRun(t, local, "rev-parse", "HEAD")
	mustRun(t, local, "push", remote, "refs/heads/main:refs/heads/main")

	mustRun(t, local, "checkout", "-b", "feature/recover")
	draftCommit := ""
	if shape.earlierPublished {
		mustWrite(t, filepath.Join(local, "draft.txt"), "earlier draft\n")
		mustRun(t, local, "add", "draft.txt")
		mustRun(t, local, "commit", "-m", "lane: earlier draft")
		draftCommit = mustRun(t, local, "rev-parse", "HEAD")
		mustRun(t, local, "push", remote, "refs/heads/feature/recover:refs/heads/feature/recover")
	}
	mustWrite(t, filepath.Join(local, "file.txt"), "lane change\nline two\n")
	mustRun(t, local, "commit", "-am", "lane: change line one")
	laneCommit := mustRun(t, local, "rev-parse", "HEAD")
	submitted := laneCommit

	// The default branch then changes the same line, so the pipeline's rebase
	// needs a conflict resolution: the replayed lane commit is a different
	// patch, and the executable three-way containment proof cannot hold.
	mustRun(t, local, "checkout", "main")
	mustWrite(t, filepath.Join(local, "file.txt"), "upstream change\nline two\n")
	mustRun(t, local, "commit", "-am", "upstream change")
	mustRun(t, local, "checkout", "feature/recover")

	gate := filepath.Join(root, "gate.git")
	mustRun(t, root, "init", "--bare", gate)
	mustRun(t, local, "push", gate, "refs/heads/main:refs/heads/main", "refs/heads/feature/recover:refs/heads/feature/recover")

	database, err := db.Open(filepath.Join(root, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(local, remote, "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature/recover", submitted, base)
	if err != nil {
		t.Fatal(err)
	}

	pipeline := filepath.Join(root, "pipeline")
	mustRun(t, root, "-c", "core.autocrlf=false", "clone", gate, pipeline)
	configureIdentity(t, pipeline)
	mustRun(t, pipeline, "checkout", "-b", "rebased", "origin/main")
	if shape.earlierPublished {
		mustRun(t, pipeline, "cherry-pick", draftCommit)
	}
	mustWrite(t, filepath.Join(pipeline, "file.txt"), "lane change over upstream\nline two\n")
	mustRun(t, pipeline, "commit", "-am", "lane: change line one")
	mustWrite(t, filepath.Join(pipeline, "fix.txt"), "review fix\n")
	mustRun(t, pipeline, "add", "fix.txt")
	mustRun(t, pipeline, "commit", "-m", "no-mistakes(review): guard the change")
	reviewFix := mustRun(t, pipeline, "rev-parse", "HEAD")
	preserved := reviewFix
	// Terminalization pins the unpublished head at the run's recovery ref; the
	// gate branch keeps the submitted head because nothing was published.
	mustRun(t, pipeline, "push", "origin", "HEAD:"+custody.RecoveryRef(run.ID))

	if err := database.UpdateRunHeadSHA(run.ID, preserved); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatusWithVerifiedHead(run.ID, shape.status, preserved); err != nil {
		t.Fatal(err)
	}
	if shape.preservedInWorktree {
		mustRun(t, local, "fetch", "--no-tags", gate, custody.RecoveryRef(run.ID)+":refs/backup/keeper-candidate")
	}
	run, _ = database.GetRun(run.ID)
	return &terminalDivergenceFixture{
		recoverFixture: &recoverFixture{
			t: t, ctx: t.Context(), db: database, repo: repo, run: run,
			service: &Service{DB: database, Repo: repo, WorkDir: local, GateDir: gate},
			local:   local, gate: gate, remote: remote,
			base: base, submitted: submitted, preserved: preserved,
		},
		laneCommit: laneCommit, draftCommit: draftCommit, reviewFix: reviewFix,
	}
}

func (f *terminalDivergenceFixture) gateBranch() string {
	f.t.Helper()
	return mustRun(f.t, f.gate, "rev-parse", "refs/heads/feature/recover")
}

func (f *terminalDivergenceFixture) adoptCommand() string {
	return "no-mistakes axi sync --recover --adopt-preserved --acknowledge " + f.submitted
}

func (f *terminalDivergenceFixture) keepLocalCommand() string {
	return "no-mistakes axi sync --recover --keep-local --acknowledge " + f.preserved
}

func (f *terminalDivergenceFixture) exitRecord() *db.CustodyExit {
	f.t.Helper()
	exit, err := f.db.GetRunCustodyExit(f.run.ID)
	if err != nil {
		f.t.Fatalf("read custody exit: %v", err)
	}
	return exit
}

// assertUntouched proves a refusal changed no ref, file, or custody row.
func (f *terminalDivergenceFixture) assertUntouched() {
	f.t.Helper()
	if got := mustRun(f.t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		f.t.Fatalf("local HEAD moved to %s", got)
	}
	if got := f.gateBranch(); got != f.submitted {
		f.t.Fatalf("gate branch moved to %s", got)
	}
	if got := mustRun(f.t, f.gate, "rev-parse", f.anchorRef()); got != f.preserved {
		f.t.Fatalf("gate recovery anchor = %s, want %s", got, f.preserved)
	}
	if f.custodyReturned() {
		f.t.Fatal("refusal stamped custody")
	}
	if exit := f.exitRecord(); exit != nil {
		f.t.Fatalf("refusal recorded a custody exit: %#v", exit)
	}
}

func commitNamed(commits []db.CustodyExitCommit, sha string) (db.CustodyExitCommit, bool) {
	for _, commit := range commits {
		if commit.SHA == sha {
			return commit, true
		}
	}
	return db.CustodyExitCommit{}, false
}

// assertDivergenceLists checks that the evidence names every commit each side
// would leave off the branch, with the patch-identity lead kept honest: the
// conflict-resolved lane commit has no match, a cleanly replayed one does.
func (f *terminalDivergenceFixture) assertDivergenceLists(localOnly, preservedOnly []db.CustodyExitCommit) {
	f.t.Helper()
	lane, ok := commitNamed(localOnly, f.laneCommit)
	if !ok {
		f.t.Fatalf("local-only commits %#v do not name the lane commit %s", localOnly, f.laneCommit)
	}
	if lane.PatchMatch || lane.Subject != "lane: change line one" || lane.PatchID == "" {
		f.t.Fatalf("lane commit evidence = %#v, want an unmatched patch identity", lane)
	}
	if f.draftCommit != "" {
		draft, ok := commitNamed(localOnly, f.draftCommit)
		if !ok || !draft.PatchMatch {
			f.t.Fatalf("cleanly replayed draft commit evidence = %#v (found %v), want a patch-identity match", draft, ok)
		}
	}
	fix, ok := commitNamed(preservedOnly, f.reviewFix)
	if !ok || fix.PatchMatch || fix.Subject != "no-mistakes(review): guard the change" {
		f.t.Fatalf("preserved-only commits %#v do not name the review fix %s", preservedOnly, f.reviewFix)
	}
}

// TestTerminalDivergenceShapesOfferAnAcknowledgedExit is the red-first
// regression for issue #233: each measured shape used to report
// blocked_recover_manual_reconciliation with inspect_and_reconcile_manually
// pointing at a read-only status, although the tool held complete evidence.
// It must now name the explicit, acknowledged exit and every commit either
// choice would leave off the branch.
func TestTerminalDivergenceShapesOfferAnAcknowledgedExit(t *testing.T) {
	t.Parallel()
	for _, shape := range terminalDivergenceShapes {
		t.Run(shape.name, func(t *testing.T) {
			t.Parallel()
			f := newTerminalDivergenceFixture(t, shape)

			state := f.service.InspectCached(f.ctx)
			if state.State != StatePipelineOwned || state.Safety != SafetyUnprovenDivergence {
				t.Fatalf("status = %s/%s (%s), want the unproven-divergence exit", state.State, state.Safety, state.Error)
			}
			if state.Relation != shape.wantRelation {
				t.Fatalf("relation = %s, want %s", state.Relation, shape.wantRelation)
			}
			if state.NextAction == nil || state.NextAction.Code != NextActionChooseCustodyExit || state.NextAction.Command != f.adoptCommand() {
				t.Fatalf("next action = %#v, want %q", state.NextAction, f.adoptCommand())
			}
			if !strings.Contains(state.Error, f.keepLocalCommand()) || !strings.Contains(state.Error, f.laneCommit) {
				t.Fatalf("status does not name the keep-local alternative and the unmatched lane commit: %q", state.Error)
			}
			evidence := state.Recovery
			if evidence == nil || evidence.Source != RecoverySourceTerminalDivergence || evidence.RunID != f.run.ID ||
				evidence.RequiredHead != f.submitted || evidence.PreservedHead != f.preserved || evidence.ArchiveRef != f.anchorRef() {
				t.Fatalf("recovery evidence = %#v", evidence)
			}
			f.assertDivergenceLists(evidence.LocalOnly, evidence.PreservedOnly)
			f.assertUntouched()
		})
	}
}

// TestTerminalDivergenceRefusesUnacknowledgedRecovery pins that neither plain
// recovery nor plain keep-local can return custody here. Keep-local used to
// succeed silently on this state although status never offered it, leaving
// the anchor holding commits the kept head lacks without anyone choosing that.
func TestTerminalDivergenceRefusesUnacknowledgedRecovery(t *testing.T) {
	t.Parallel()
	for _, shape := range terminalDivergenceShapes {
		t.Run(shape.name, func(t *testing.T) {
			t.Parallel()
			f := newTerminalDivergenceFixture(t, shape)
			for _, keepLocal := range []bool{false, true} {
				state := f.service.Recover(f.ctx, keepLocal)
				if state.Recovered || state.Safety != SafetyUnprovenDivergence {
					t.Fatalf("Recover(keepLocal=%v) = %s recovered=%v (%s), want the acknowledgement refusal", keepLocal, state.Safety, state.Recovered, state.Error)
				}
				if state.NextAction == nil || state.NextAction.Code != NextActionChooseCustodyExit {
					t.Fatalf("Recover(keepLocal=%v) next action = %#v", keepLocal, state.NextAction)
				}
				if !strings.Contains(state.Error, f.laneCommit) || !strings.Contains(state.Error, f.reviewFix) {
					t.Fatalf("Recover(keepLocal=%v) refusal does not name the commits at stake: %q", keepLocal, state.Error)
				}
				f.assertUntouched()
			}
		})
	}
}

func TestTerminalDivergenceAdoptPreservedWithAcknowledgement(t *testing.T) {
	t.Parallel()
	for _, shape := range terminalDivergenceShapes {
		t.Run(shape.name, func(t *testing.T) {
			t.Parallel()
			f := newTerminalDivergenceFixture(t, shape)
			request := CustodyExitRequest{Mode: CustodyExitAdoptPreserved, Acknowledge: f.preserved, Invoker: "test-owner"}

			// The acknowledgement names the head being set aside; naming the
			// head being taken is a different decision and refuses.
			wrong := f.service.ExitTerminalDivergence(f.ctx, request)
			if wrong.Recovered || wrong.Safety != "blocked_recover_acknowledgement_mismatch" || !strings.Contains(wrong.Error, f.submitted) {
				t.Fatalf("mismatched acknowledgement = %s (%s)", wrong.Safety, wrong.Error)
			}
			f.assertUntouched()

			request.Acknowledge = f.submitted
			adopted := f.service.ExitTerminalDivergence(f.ctx, request)
			if !adopted.Recovered || !adopted.Changed {
				t.Fatalf("acknowledged adopt = %s recovered=%v changed=%v (%s)", adopted.Safety, adopted.Recovered, adopted.Changed, adopted.Error)
			}
			if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.preserved {
				t.Fatalf("local HEAD = %s, want preserved %s", got, f.preserved)
			}
			if status := mustRun(t, f.local, "status", "--porcelain"); status != "" {
				t.Fatalf("worktree not clean after adopt: %q", status)
			}
			if got := readOptional(t, filepath.Join(f.local, "fix.txt")); got != "review fix\n" {
				t.Fatalf("worktree does not carry the preserved content: %q", got)
			}
			// The gate follows the adopted head, so the next launch is not
			// refused by a private mirror still at the submitted head (#163).
			if got := f.gateBranch(); got != f.preserved {
				t.Fatalf("gate branch = %s, want preserved %s", got, f.preserved)
			}
			// Nothing is lost: the set-aside submitted head stays anchored in
			// the worktree and the gate, and the preserved anchor stays put.
			if got := mustRun(t, f.local, "rev-parse", f.localAnchorRef()); got != f.submitted {
				t.Fatalf("worktree pre-recovery anchor = %s, want %s", got, f.submitted)
			}
			if got := mustRun(t, f.gate, "rev-parse", custody.RecoveryGateRef(f.run.ID)); got != f.submitted {
				t.Fatalf("gate pre-recovery anchor = %s, want %s", got, f.submitted)
			}
			for _, dir := range []string{f.local, f.gate} {
				if got := mustRun(t, dir, "rev-parse", f.anchorRef()); got != f.preserved {
					t.Fatalf("%s recovery anchor = %s, want %s", dir, got, f.preserved)
				}
			}
			if !f.custodyReturned() {
				t.Fatal("acknowledged adopt did not return custody")
			}
			exit := f.exitRecord()
			if exit == nil || exit.Mode != string(CustodyExitAdoptPreserved) || exit.KeptHead != f.preserved ||
				exit.SetAsideHead != f.submitted || exit.Acknowledged != f.submitted || exit.Invoker != "test-owner" {
				t.Fatalf("custody exit record = %#v", exit)
			}
			f.assertDivergenceLists(exit.LocalOnly, exit.PreservedOnly)

			// Idempotent: a repeat neither moves anything nor records twice.
			again := f.service.ExitTerminalDivergence(f.ctx, request)
			if !again.Recovered || again.Changed {
				t.Fatalf("repeated adopt = %s recovered=%v changed=%v (%s)", again.Safety, again.Recovered, again.Changed, again.Error)
			}
			if repeat := f.exitRecord(); repeat == nil || repeat.RecordedAt != exit.RecordedAt {
				t.Fatalf("repeat rewrote the custody exit record: %#v", repeat)
			}
		})
	}
}

func TestTerminalDivergenceKeepLocalWithAcknowledgement(t *testing.T) {
	t.Parallel()
	for _, shape := range terminalDivergenceShapes {
		t.Run(shape.name, func(t *testing.T) {
			t.Parallel()
			f := newTerminalDivergenceFixture(t, shape)
			request := CustodyExitRequest{Mode: CustodyExitKeepLocal, Acknowledge: f.submitted, Invoker: "test-owner"}
			if wrong := f.service.ExitTerminalDivergence(f.ctx, request); wrong.Recovered || wrong.Safety != "blocked_recover_acknowledgement_mismatch" {
				t.Fatalf("mismatched keep-local acknowledgement = %s (%s)", wrong.Safety, wrong.Error)
			}
			f.assertUntouched()

			request.Acknowledge = f.preserved
			kept := f.service.ExitTerminalDivergence(f.ctx, request)
			if !kept.Recovered || kept.Changed {
				t.Fatalf("acknowledged keep-local = %s recovered=%v changed=%v (%s)", kept.Safety, kept.Recovered, kept.Changed, kept.Error)
			}
			if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
				t.Fatalf("keep-local moved HEAD to %s", got)
			}
			if got := f.gateBranch(); got != f.submitted {
				t.Fatalf("gate branch = %s, want the kept head %s", got, f.submitted)
			}
			// The preserved commits stay inspectable from the worktree with
			// `git log <anchor> --not <kept>`, even where the worktree never
			// had the object before.
			for _, dir := range []string{f.local, f.gate} {
				if got := mustRun(t, dir, "rev-parse", f.anchorRef()); got != f.preserved {
					t.Fatalf("%s recovery anchor = %s, want %s", dir, got, f.preserved)
				}
			}
			if !f.custodyReturned() {
				t.Fatal("acknowledged keep-local did not return custody")
			}
			exit := f.exitRecord()
			if exit == nil || exit.Mode != string(CustodyExitKeepLocal) || exit.KeptHead != f.submitted ||
				exit.SetAsideHead != f.preserved || exit.Acknowledged != f.preserved {
				t.Fatalf("custody exit record = %#v", exit)
			}
			f.assertDivergenceLists(exit.LocalOnly, exit.PreservedOnly)
		})
	}
}

// TestTerminalDivergenceFailsClosedNamingTheMissingProof keeps every
// precondition load-bearing: each missing or conflicting fact keeps the
// manual-reconciliation refusal, names the fact, and refuses an acknowledged
// exit without changing anything.
func TestTerminalDivergenceFailsClosedNamingTheMissingProof(t *testing.T) {
	t.Parallel()
	shape := terminalDivergenceShapes[1]
	cases := []struct {
		name   string
		mutate func(f *terminalDivergenceFixture)
		want   string
		// localHead and gateHead return the heads the mutation left, so the
		// no-mutation check compares against them.
		localHead func(f *terminalDivergenceFixture) string
		gateHead  func(f *terminalDivergenceFixture) string
	}{
		{
			name: "dirty_worktree",
			mutate: func(f *terminalDivergenceFixture) {
				mustWrite(f.t, filepath.Join(f.local, "file.txt"), "uncommitted\n")
			},
			want: "worktree is not clean",
		},
		{
			name: "local_moved_off_submitted",
			mutate: func(f *terminalDivergenceFixture) {
				mustWrite(f.t, filepath.Join(f.local, "later.txt"), "later\n")
				mustRun(f.t, f.local, "add", "later.txt")
				mustRun(f.t, f.local, "commit", "-m", "later local work")
			},
			want:      "not the run's submitted head",
			localHead: func(f *terminalDivergenceFixture) string { return mustRun(f.t, f.local, "rev-parse", "HEAD") },
		},
		{
			name: "gate_anchor_missing",
			mutate: func(f *terminalDivergenceFixture) {
				mustRun(f.t, f.gate, "update-ref", "-d", f.anchorRef())
			},
			want: "has no recovery ref",
		},
		{
			name: "gate_branch_moved_elsewhere",
			mutate: func(f *terminalDivergenceFixture) {
				mustRun(f.t, f.gate, "update-ref", "refs/heads/feature/recover", f.base)
			},
			want:     "gate branch",
			gateHead: func(f *terminalDivergenceFixture) string { return f.base },
		},
		{
			name: "run_published_a_head",
			mutate: func(f *terminalDivergenceFixture) {
				if err := f.db.UpdateRunPushBinding(f.run.ID, db.PushBinding{
					HeadSHA: f.base, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(f.remote), Ref: "refs/heads/feature/recover",
				}); err != nil {
					f.t.Fatal(err)
				}
			},
			want: "published",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newTerminalDivergenceFixture(t, shape)
			tc.mutate(f)
			wantLocal, wantGate := f.submitted, f.submitted
			if tc.localHead != nil {
				wantLocal = tc.localHead(f)
			}
			if tc.gateHead != nil {
				wantGate = tc.gateHead(f)
			}

			state := f.service.InspectCached(f.ctx)
			if state.Safety == SafetyUnprovenDivergence || state.Recovered {
				t.Fatalf("status offered the acknowledged exit despite %s: %#v", tc.name, state)
			}
			if state.Safety == "blocked_recover_manual_reconciliation" && !strings.Contains(state.Error, tc.want) {
				t.Fatalf("manual refusal does not name %q: %q", tc.want, state.Error)
			}
			for _, request := range []CustodyExitRequest{
				{Mode: CustodyExitAdoptPreserved, Acknowledge: f.submitted},
				{Mode: CustodyExitKeepLocal, Acknowledge: f.preserved},
			} {
				refused := f.service.ExitTerminalDivergence(f.ctx, request)
				if refused.Recovered || refused.Safety != "blocked_recover_acknowledgement_not_applicable" {
					t.Fatalf("%s exit = %s recovered=%v (%s)", request.Mode, refused.Safety, refused.Recovered, refused.Error)
				}
			}
			if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != wantLocal {
				t.Fatalf("local HEAD moved to %s", got)
			}
			if got := f.gateBranch(); got != wantGate {
				t.Fatalf("gate branch moved to %s", got)
			}
			if f.custodyReturned() || f.exitRecord() != nil {
				t.Fatal("refusal returned custody or recorded an exit")
			}
		})
	}
}

// TestTerminalDivergenceAdoptRollsBackTheGateWhenTheWorktreeMoves proves the
// adopt path fails closed under a concurrent local commit: the gate branch
// moves first, so a worktree that changed afterwards must put it back, keep the
// new commit, and record nothing.
func TestTerminalDivergenceAdoptRollsBackTheGateWhenTheWorktreeMoves(t *testing.T) {
	t.Parallel()
	f := newTerminalDivergenceFixture(t, terminalDivergenceShapes[1])
	var concurrent string
	f.service.beforeRecoverWorktreeMove = func() {
		mustWrite(t, filepath.Join(f.local, "concurrent.txt"), "concurrent\n")
		mustRun(t, f.local, "add", "concurrent.txt")
		mustRun(t, f.local, "commit", "-m", "concurrent local commit")
		concurrent = mustRun(t, f.local, "rev-parse", "HEAD")
	}
	refused := f.service.ExitTerminalDivergence(f.ctx, CustodyExitRequest{Mode: CustodyExitAdoptPreserved, Acknowledge: f.submitted})
	if refused.Recovered || refused.Safety != "blocked_recover_assumptions_changed" {
		t.Fatalf("adopt under a concurrent commit = %s recovered=%v (%s)", refused.Safety, refused.Recovered, refused.Error)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != concurrent {
		t.Fatalf("concurrent commit lost: HEAD = %s, want %s", got, concurrent)
	}
	if got := f.gateBranch(); got != f.submitted {
		t.Fatalf("gate branch = %s, want it restored to %s", got, f.submitted)
	}
	if f.custodyReturned() || f.exitRecord() != nil {
		t.Fatal("refused adopt returned custody or recorded an exit")
	}
}

func TestTerminalDivergencePlainKeepLocalRefusesIncompleteEvidence(t *testing.T) {
	t.Parallel()
	for _, missing := range []string{"gate_branch", "gate_anchor"} {
		t.Run(missing, func(t *testing.T) {
			t.Parallel()
			f := newTerminalDivergenceFixture(t, terminalDivergenceShapes[1])
			wantGate, wantReason := f.submitted, "has no recovery ref"
			if missing == "gate_branch" {
				wantGate, wantReason = f.base, "gate branch"
				mustRun(t, f.gate, "update-ref", "refs/heads/feature/recover", f.base)
			} else {
				mustRun(t, f.gate, "update-ref", "-d", f.anchorRef())
			}
			refused := f.service.Recover(f.ctx, true)
			if refused.Recovered || refused.Safety != "blocked_recover_manual_reconciliation" || !strings.Contains(refused.Error, wantReason) {
				t.Fatalf("incomplete evidence recovery = %#v, want manual refusal naming %q", refused, wantReason)
			}
			for _, fact := range []string{f.submitted, f.preserved, "git merge-base --is-ancestor", "git merge-tree --write-tree --merge-base"} {
				if !strings.Contains(refused.Error, fact) {
					t.Fatalf("manual refusal omits diagnostic %q: %s", fact, refused.Error)
				}
			}
			if refused.NextAction == nil || refused.NextAction.Code != "inspect_and_reconcile_manually" {
				t.Fatalf("next action = %#v", refused.NextAction)
			}
			if got := f.gateBranch(); got != wantGate {
				t.Fatalf("gate branch changed to %s, want %s", got, wantGate)
			}
			if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
				t.Fatalf("local head changed to %s", got)
			}
			if f.custodyReturned() || f.exitRecord() != nil {
				t.Fatal("refusal returned custody or recorded an exit")
			}
		})
	}
}

func TestTerminalDivergenceAdoptRestoresBranchesWhenReceiptFails(t *testing.T) {
	t.Parallel()
	for _, gateAlreadyPreserved := range []bool{false, true} {
		t.Run(fmt.Sprint(gateAlreadyPreserved), func(t *testing.T) {
			t.Parallel()
			f := newTerminalDivergenceFixture(t, terminalDivergenceShapes[1])
			wantGate := f.submitted
			if gateAlreadyPreserved {
				wantGate = f.preserved
				mustRun(t, f.gate, "update-ref", "refs/heads/feature/recover", wantGate)
			}
			f.service.beforeCustodyExitStamp = func() {
				if err := f.db.SetRunCustodyReturned(f.run.ID); err != nil {
					t.Fatal(err)
				}
			}
			refused := f.service.ExitTerminalDivergence(f.ctx, CustodyExitRequest{Mode: CustodyExitAdoptPreserved, Acknowledge: f.submitted})
			if refused.Recovered || refused.Changed || refused.Safety != "blocked_recover_stamp_failed" {
				t.Fatalf("failed receipt adoption = %#v", refused)
			}
			if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
				t.Fatalf("local branch = %s, want submitted %s", got, f.submitted)
			}
			if got := f.gateBranch(); got != wantGate {
				t.Fatalf("gate branch = %s, want original %s", got, wantGate)
			}
			if got := mustRun(t, f.local, "status", "--porcelain"); got != "" {
				t.Fatalf("rollback left dirty worktree: %s", got)
			}
			if got := readOptional(t, filepath.Join(f.local, "file.txt")); got != "lane change\nline two\n" {
				t.Fatalf("rollback content = %q", got)
			}
			for _, dir := range []string{f.local, f.gate} {
				if got := mustRun(t, dir, "rev-parse", f.anchorRef()); got != f.preserved {
					t.Fatalf("%s lost preserved anchor: %s", dir, got)
				}
			}
			if got := mustRun(t, f.local, "rev-parse", f.localAnchorRef()); got != f.submitted {
				t.Fatalf("lost local anchor: %s", got)
			}
			if !gateAlreadyPreserved {
				if got := mustRun(t, f.gate, "rev-parse", custody.RecoveryGateRef(f.run.ID)); got != f.submitted {
					t.Fatalf("lost gate anchor: %s", got)
				}
			}
			if f.exitRecord() != nil {
				t.Fatal("failed receipt wrote an exit")
			}
		})
	}
}

func TestTerminalDivergenceListsEveryCommitInStatusAndReceipt(t *testing.T) {
	t.Parallel()
	f := newTerminalDivergenceFixture(t, terminalDivergenceShapes[1])
	tree := mustRun(t, f.gate, "rev-parse", f.preserved+"^{tree}")
	configureIdentity(t, f.gate)
	head := f.preserved
	var added []string
	for i := 0; i < 201; i++ {
		head = mustRun(t, f.gate, "commit-tree", tree, "-p", head, "-m", fmt.Sprintf("preserved %d", i))
		added = append(added, head)
	}
	mustRun(t, f.gate, "update-ref", f.anchorRef(), head, f.preserved)
	if err := f.db.UpdateRunHeadSHA(f.run.ID, head); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunStatusWithVerifiedHead(f.run.ID, types.RunCancelled, head); err != nil {
		t.Fatal(err)
	}
	state := f.service.InspectCached(f.ctx)
	if state.Recovery == nil || state.Safety != SafetyUnprovenDivergence {
		t.Fatalf("status = %#v", state)
	}
	for _, sha := range added {
		if _, ok := commitNamed(state.Recovery.PreservedOnly, sha); !ok {
			t.Fatalf("status omitted preserved commit %s", sha)
		}
	}
	if state.Recovery.PreservedOnlyTotal != len(state.Recovery.PreservedOnly) {
		t.Fatal("status list does not match full count")
	}
	result := f.service.ExitTerminalDivergence(f.ctx, CustodyExitRequest{Mode: CustodyExitKeepLocal, Acknowledge: head})
	if !result.Recovered {
		t.Fatalf("keep-local = %#v", result)
	}
	exit := f.exitRecord()
	if exit == nil || exit.PreservedOnlyTotal != len(exit.PreservedOnly) {
		t.Fatalf("receipt = %#v", exit)
	}
	for _, sha := range added {
		if _, ok := commitNamed(exit.PreservedOnly, sha); !ok {
			t.Fatalf("receipt omitted preserved commit %s", sha)
		}
	}
}

func TestTerminalDivergenceAdoptRollsBackDirtyWorktreeAndCanRetry(t *testing.T) {
	t.Parallel()
	for _, gateAlreadyPreserved := range []bool{false, true} {
		t.Run(fmt.Sprint(gateAlreadyPreserved), func(t *testing.T) {
			t.Parallel()
			f := newTerminalDivergenceFixture(t, terminalDivergenceShapes[1])
			wantGate := f.submitted
			if gateAlreadyPreserved {
				wantGate = f.preserved
				mustRun(t, f.gate, "update-ref", "refs/heads/feature/recover", wantGate)
			}
			unrelated := filepath.Join(f.local, "unrelated.txt")
			f.service.afterRecoverBranchMove = func() {
				mustWrite(t, unrelated, "unrelated work\n")
			}
			request := CustodyExitRequest{Mode: CustodyExitAdoptPreserved, Acknowledge: f.submitted, Invoker: "test-owner"}
			refused := f.service.ExitTerminalDivergence(f.ctx, request)
			if refused.Recovered || refused.Changed {
				t.Fatalf("dirty adoption = %#v, want refusal with branch restored", refused)
			}
			if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
				t.Fatalf("local HEAD = %s, want submitted %s", got, f.submitted)
			}
			if got := f.gateBranch(); got != wantGate {
				t.Fatalf("gate branch = %s, want pre-exit %s", got, wantGate)
			}
			if got := readOptional(t, unrelated); got != "unrelated work\n" {
				t.Fatalf("unrelated work changed: %q", got)
			}
			if got := readOptional(t, filepath.Join(f.local, "file.txt")); got != "lane change\nline two\n" {
				t.Fatalf("submitted worktree content was not restored: %q", got)
			}
			if f.custodyReturned() || f.exitRecord() != nil {
				t.Fatal("unsuccessful adoption returned custody or recorded an exit")
			}
			for _, dir := range []string{f.local, f.gate} {
				if got := mustRun(t, dir, "rev-parse", f.anchorRef()); got != f.preserved {
					t.Fatalf("%s lost preserved anchor: %s", dir, got)
				}
			}
			if got := mustRun(t, f.local, "rev-parse", f.localAnchorRef()); got != f.submitted {
				t.Fatalf("lost local anchor: %s", got)
			}
			if !gateAlreadyPreserved {
				if got := mustRun(t, f.gate, "rev-parse", custody.RecoveryGateRef(f.run.ID)); got != f.submitted {
					t.Fatalf("lost gate anchor: %s", got)
				}
			}
			if err := os.Remove(unrelated); err != nil {
				t.Fatal(err)
			}
			f.service.afterRecoverBranchMove = nil
			if state := f.service.InspectCached(f.ctx); state.Safety != SafetyUnprovenDivergence {
				t.Fatalf("after rollback and removing unrelated work: %#v", state)
			}
			retried := f.service.ExitTerminalDivergence(f.ctx, request)
			if !retried.Recovered || !retried.Changed || !f.custodyReturned() {
				t.Fatalf("same acknowledged adoption could not be retried: %#v", retried)
			}
			if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.preserved {
				t.Fatalf("retry local HEAD = %s, want preserved %s", got, f.preserved)
			}
			if got := f.gateBranch(); got != f.preserved {
				t.Fatalf("retry gate branch = %s, want preserved %s", got, f.preserved)
			}
			if exit := f.exitRecord(); exit == nil || exit.Mode != string(request.Mode) || exit.Acknowledged != f.submitted || exit.KeptHead != f.preserved {
				t.Fatalf("retry receipt = %#v", exit)
			}
		})
	}
}
