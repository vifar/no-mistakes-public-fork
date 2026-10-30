package branchsync

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
)

// This file owns the acknowledged custody exit for issue #233: a terminal run
// that published nothing, whose clean registered worktree still sits at the
// run's submitted head, and whose preserved head - pinned by terminalization at
// refs/no-mistakes/recover/<run> - diverged from it without a containment
// proof. A conflict-resolved rebase, or a fix round that rewrote the
// operator's own lines, leaves exactly that state, and before this exit it had
// no supported way out: status pointed at inspect_and_reconcile_manually and a
// read-only `axi status`, although the tool held every fact needed to decide.
//
// The exit does not relax the containment proof. Nothing here decides that one
// head carries the other's work. Instead it names both choices, lists every
// commit each would leave off the branch, and requires the operator to
// acknowledge the exact head being set aside. Anchoring is preservation, not
// continuation: both exits keep every commit reachable from a tool-owned ref,
// and the acknowledgement, with the lists, is recorded in the run as it
// returns custody. Everything short of complete evidence fails closed and
// names the missing fact.

const (
	// SafetyUnprovenDivergence reports the stranded state the acknowledged
	// exit serves. It is a refusal: plain recovery never chooses a head here.
	SafetyUnprovenDivergence = "blocked_recover_unproven_divergence"
	// NextActionChooseCustodyExit asks the operator to pick one of the two
	// acknowledged exits. Its command is the adopt form; the keep-local form
	// is named in the state's note.
	NextActionChooseCustodyExit = "choose_custody_exit"
	// RecoverySourceTerminalDivergence labels the evidence behind the exit.
	RecoverySourceTerminalDivergence = "terminal_divergence"
)

// CustodyExitMode selects which head the branch keeps.
type CustodyExitMode string

const (
	// CustodyExitKeepLocal keeps the submitted local head and leaves the
	// preserved commits at the run's recovery anchor.
	CustodyExitKeepLocal CustodyExitMode = "keep_local"
	// CustodyExitAdoptPreserved moves the worktree and the gate branch to the
	// preserved head and anchors the submitted head.
	CustodyExitAdoptPreserved CustodyExitMode = "adopt_preserved"
)

// CustodyExitRequest is the operator's acknowledged choice. Acknowledge must
// be the exact full SHA of the head being set aside, so an acknowledgement
// given for one state can never authorize a different one.
type CustodyExitRequest struct {
	Mode        CustodyExitMode
	Acknowledge string
	// Invoker identifies who chose, for the recorded exit.
	Invoker string
}

// terminalDivergence is the complete, freshly read evidence for the exit.
type terminalDivergence struct {
	repoID    string
	runID     string
	status    string
	branch    string
	local     string
	preserved string
	gateHead  string
	anchorRef string

	localOnly          []db.CustodyExitCommit
	preservedOnly      []db.CustodyExitCommit
	localOnlyTotal     int
	preservedOnlyTotal int
}

// discoverTerminalDivergence returns the exit's evidence, or the first missing
// or conflicting fact. It is read-only: it never fetches, anchors, or writes.
// recoverySourceAvailable calls it only after ordinary, archive-backed, and
// preserved-rebase recovery are all unavailable, so ancestry and the merge
// proof have already failed when it runs.
func (s *Service) discoverTerminalDivergence(ctx context.Context, state State, run *db.Run) (*terminalDivergence, string) {
	switch {
	case run.TerminalHeadVerifiedAt == nil:
		return nil, "the run's terminal head was never verified"
	case run.LastPushedSHA != nil || value(run.PushGeneration) > 0:
		return nil, "the run published a head, so its push binding owns recovery"
	case run.SubmittedHeadSHA == nil:
		return nil, "the run recorded no submitted head"
	case !state.Local.Clean:
		return nil, fmt.Sprintf("the invoking worktree is not clean (%s)", state.Local.Reason)
	case state.Local.Head != *run.SubmittedHeadSHA:
		return nil, fmt.Sprintf("local head %s is not the run's submitted head %s", state.Local.Head, *run.SubmittedHeadSHA)
	}
	gateDir := strings.TrimSpace(s.GateDir)
	if gateDir == "" {
		return nil, "no local gate is configured"
	}
	if _, err := os.Stat(gateDir); err != nil {
		return nil, "the local gate is unavailable"
	}
	local, preserved, branch := state.Local.Head, run.HeadSHA, state.Local.Branch
	anchorRef := custody.RecoveryRef(run.ID)
	anchored, exists, err := git.DirectRefTarget(ctx, gateDir, anchorRef)
	switch {
	case err != nil:
		return nil, fmt.Sprintf("the local gate recovery ref %s could not be read as a direct ref", anchorRef)
	case !exists:
		return nil, fmt.Sprintf("the local gate has no recovery ref %s for the run", anchorRef)
	case anchored != preserved:
		return nil, fmt.Sprintf("the local gate recovery ref %s points at %s, not the recorded head %s", anchorRef, anchored, preserved)
	}
	if resolved, err := git.Run(ctx, gateDir, "rev-parse", "--verify", preserved+"^{commit}"); err != nil || resolved != preserved {
		return nil, fmt.Sprintf("the preserved head %s is not a commit in the local gate", preserved)
	}
	if !objectExists(ctx, gateDir, local) {
		return nil, fmt.Sprintf("the submitted head %s is not in the local gate", local)
	}
	gateHead, gateExists, err := git.DirectRefTarget(ctx, gateDir, "refs/heads/"+branch)
	if err != nil || !gateExists || (gateHead != local && gateHead != preserved) {
		return nil, fmt.Sprintf("the gate branch %s is not at the submitted head %s or the preserved head %s", branch, local, preserved)
	}
	if isAncestor(ctx, gateDir, local, preserved) || isAncestor(ctx, gateDir, preserved, local) {
		return nil, "the submitted and preserved heads are related by ancestry"
	}
	if run.ReviewApprovedHeadSHA != nil && *run.ReviewApprovedHeadSHA != preserved &&
		reviewedHeadProvesEquivalentTarget(ctx, gateDir, local, *run.ReviewApprovedHeadSHA, preserved) {
		return nil, "the reviewed head proves the preserved head carries the local work, so plain `no-mistakes axi sync --recover` adopts it"
	}
	localOnly, localTotal, localErr := listDivergentCommits(ctx, gateDir, local, preserved)
	preservedOnly, preservedTotal, preservedErr := listDivergentCommits(ctx, gateDir, preserved, local)
	if localErr != nil || preservedErr != nil {
		return nil, "the commits each head lacks could not be listed"
	}
	markPatchMatches(localOnly, preservedOnly)
	return &terminalDivergence{
		repoID: s.Repo.ID, runID: run.ID, status: string(run.Status), branch: branch,
		local: local, preserved: preserved, gateHead: gateHead, anchorRef: anchorRef,
		localOnly: localOnly, preservedOnly: preservedOnly,
		localOnlyTotal: localTotal, preservedOnlyTotal: preservedTotal,
	}, ""
}

// listDivergentCommits names the commits reachable from include but not from
// exclude, newest first, with each non-merge commit's stable patch identity.
// The identity is measured with the same settings as the diverged-refusal
// diagnostic (rename detection on, text conversion and external diff off) so
// both sides of a comparison describe one change the same way.
func listDivergentCommits(ctx context.Context, dir, include, exclude string) ([]db.CustodyExitCommit, int, error) {
	count, err := git.Run(ctx, dir, "rev-list", "--count", include, "^"+exclude)
	if err != nil {
		return nil, 0, err
	}
	total, err := strconv.Atoi(strings.TrimSpace(count))
	if err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return nil, 0, nil
	}
	listed, err := git.Run(ctx, dir, "log", "--no-color", "--format=%H%x09%s", include, "^"+exclude)
	if err != nil {
		return nil, 0, err
	}
	var commits []db.CustodyExitCommit
	var shas []string
	for _, line := range strings.Split(listed, "\n") {
		sha, subject, _ := strings.Cut(strings.TrimSpace(line), "\t")
		if sha == "" {
			continue
		}
		commits = append(commits, db.CustodyExitCommit{SHA: sha, Subject: subject})
		shas = append(shas, sha)
	}
	args := append([]string{"--no-pager", "log", "--no-walk=unsorted", "--no-color", "--submodule=short", "--ignore-submodules=none",
		"--format=commit %H", "-p", "-M", "--no-textconv", "--no-ext-diff"}, shas...)
	stream, err := git.Run(ctx, dir, args...)
	if err != nil {
		return nil, 0, err
	}
	identities, err := git.StablePatchIdentities(ctx, dir, stream)
	if err != nil {
		return nil, 0, err
	}
	patchIDs := make(map[string]string, len(identities))
	for _, identity := range identities {
		patchIDs[identity.Commit] = identity.PatchID
	}
	for i := range commits {
		commits[i].PatchID = patchIDs[commits[i].SHA]
	}
	return commits, total, nil
}

// markPatchMatches flags each commit whose patch identity appears on the other
// side. A match is a lead for the reader, never a containment proof.
func markPatchMatches(a, b []db.CustodyExitCommit) {
	inA, inB := map[string]bool{}, map[string]bool{}
	for _, commit := range a {
		inA[commit.PatchID] = commit.PatchID != ""
	}
	for _, commit := range b {
		inB[commit.PatchID] = commit.PatchID != ""
	}
	for i := range a {
		a[i].PatchMatch = inB[a[i].PatchID]
	}
	for i := range b {
		b[i].PatchMatch = inA[b[i].PatchID]
	}
}

func (d *terminalDivergence) adoptCommand() string {
	return "no-mistakes axi sync --recover --adopt-preserved --acknowledge " + d.local
}

func (d *terminalDivergence) keepLocalCommand() string {
	return "no-mistakes axi sync --recover --keep-local --acknowledge " + d.preserved
}

func (d *terminalDivergence) evidence(proof string, keepLocal bool) *RecoveryEvidence {
	return &RecoveryEvidence{
		Source: RecoverySourceTerminalDivergence, RepositoryID: d.repoID, RunID: d.runID, Branch: d.branch,
		RequiredHead: d.local, PreservedHead: d.preserved, ArchiveRef: d.anchorRef, KeepLocal: keepLocal, Proof: proof,
		LocalOnly: d.localOnly, PreservedOnly: d.preservedOnly,
		LocalOnlyTotal: d.localOnlyTotal, PreservedOnlyTotal: d.preservedOnlyTotal,
	}
}

// apply renders the refusal that offers both exits. Status and every
// unacknowledged Recover return it unchanged.
func (d *terminalDivergence) apply(state State) State {
	state.State = StatePipelineOwned
	state.Safety = SafetyUnprovenDivergence
	state.Recovery = d.evidence("unproven", false)
	state.Error = fmt.Sprintf("the run finished %s without publishing, and its preserved head %s (anchored at %s) diverges from the submitted local head %s: "+
		"neither is an ancestor of the other and the three-way merge proof does not show the preserved head carries the local work, so recovery will not choose a head. "+
		"Both exits keep every commit reachable and record the acknowledgement in the run. "+
		"Adopt the preserved head with `%s`: the worktree and gate branch move to %s, and the submitted head stays anchored at %s and %s; local commits the preserved head does not carry: %s. "+
		"Or keep the local head with `%s`: the branch stays at %s, and the preserved commits stay at %s; preserved commits the local head does not carry: %s. "+
		"A patch-ID match is a lead for the reader, not a containment proof; no files or refs were changed",
		d.status, d.preserved, d.anchorRef, d.local,
		d.adoptCommand(), d.preserved, custody.RecoveryLocalRef(d.runID), custody.RecoveryGateRef(d.runID), describeCommits(d.localOnly, d.localOnlyTotal),
		d.keepLocalCommand(), d.local, d.anchorRef, describeCommits(d.preservedOnly, d.preservedOnlyTotal))
	state.NextAction = &NextAction{Code: NextActionChooseCustodyExit, Command: d.adoptCommand()}
	return state
}

// divergenceNoteLimit bounds how many commits the note names per side. The
// structured lists in RecoveryEvidence carry every listed commit.
const divergenceNoteLimit = 20

// describeCommits renders one side for the note: every commit with no patch-ID
// match on the other side is named, up to divergenceNoteLimit, because those
// are the ones no lead accounts for; the rest are counted.
func describeCommits(commits []db.CustodyExitCommit, total int) string {
	if total == 0 {
		return "none"
	}
	var named []string
	matched, unnamed := 0, 0
	for _, commit := range commits {
		if commit.PatchMatch {
			matched++
			continue
		}
		if len(named) == divergenceNoteLimit {
			unnamed++
			continue
		}
		patch := "merge or empty commit, no patch ID"
		if commit.PatchID != "" {
			patch = "patch-id " + commit.PatchID[:min(12, len(commit.PatchID))] + ", no match on the other side"
		}
		named = append(named, fmt.Sprintf("%s %q (%s)", commit.SHA, commit.Subject, patch))
	}
	summary := fmt.Sprintf("%d commit(s)", total)
	if len(named) > 0 {
		summary += ": " + strings.Join(named, "; ")
	}
	var rest []string
	if unnamed > 0 {
		rest = append(rest, fmt.Sprintf("%d more without a patch-ID match", unnamed))
	}
	if matched > 0 {
		rest = append(rest, fmt.Sprintf("%d with a patch-ID match on the other side", matched))
	}
	if len(rest) > 0 {
		summary += "; plus " + strings.Join(rest, ", ")
	}
	return summary + " (all commits are listed in branch_sync.recovery)"
}

// ExitTerminalDivergence performs the operator's acknowledged custody exit.
// It applies only while status reports SafetyUnprovenDivergence, re-reads all
// evidence first, and requires the acknowledgement to name the exact head being
// set aside: the preserved head for keep-local, the submitted local head for
// adopt. Repeating a completed exit is an idempotent no-op that records
// nothing.
func (s *Service) ExitTerminalDivergence(ctx context.Context, request CustodyExitRequest) State {
	if refusal, blocked := s.gateContextRefusal(ctx); blocked {
		return refusal
	}
	state, run, _ := s.inspect(ctx)
	if run != nil && run.CustodyReturnedAt != nil {
		exit, err := s.DB.GetRunCustodyExit(run.ID)
		if err == nil && exit != nil && exit.Mode == string(request.Mode) && exit.Acknowledged == request.Acknowledge {
			state.Recovered = true
			state.Changed = false
			return state
		}
		return blockedPlan(state, state.State, "blocked_recover_acknowledgement_not_applicable", "custody of this branch was already returned without this acknowledged exit; no files or refs were changed")
	}
	if state.Safety != SafetyUnprovenDivergence || run == nil {
		refused := state
		refused.Safety = "blocked_recover_acknowledgement_not_applicable"
		refused.Error = "an acknowledged custody exit applies only while status reports " + SafetyUnprovenDivergence + "; the branch reports " + state.Safety
		if state.Error != "" {
			refused.Error += " (" + state.Error + ")"
		}
		refused.Error += "; no files or refs were changed"
		return refused
	}
	divergence, reason := s.discoverTerminalDivergence(ctx, state, run)
	if divergence == nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_acknowledgement_not_applicable", "the acknowledged custody exit is no longer available because "+reason+"; no files or refs were changed")
	}
	var setAside string
	switch request.Mode {
	case CustodyExitKeepLocal:
		setAside = divergence.preserved
	case CustodyExitAdoptPreserved:
		setAside = divergence.local
	default:
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_acknowledgement_invalid", fmt.Sprintf("unknown custody exit %q; no files or refs were changed", request.Mode))
	}
	if request.Acknowledge != setAside {
		blocked := divergence.apply(state)
		blocked.Safety = "blocked_recover_acknowledgement_mismatch"
		blocked.Error = fmt.Sprintf("%s sets aside %s, so its acknowledgement must name exactly that head, not %q; %s", request.Mode, setAside, request.Acknowledge, blocked.Error)
		return blocked
	}
	exit := &db.CustodyExit{
		Mode: string(request.Mode), SetAsideHead: setAside, Acknowledged: request.Acknowledge, Invoker: request.Invoker,
		Anchors:   []string{divergence.anchorRef},
		LocalOnly: divergence.localOnly, PreservedOnly: divergence.preservedOnly,
		LocalOnlyTotal: divergence.localOnlyTotal, PreservedOnlyTotal: divergence.preservedOnlyTotal,
	}
	// Both exits leave the preserved commits inspectable from the worktree
	// (`git log <anchor> --not <kept>`), and adopting needs the object there.
	if blocked, ok := s.anchorPreservedInWorktree(ctx, state, divergence); !ok {
		return blocked
	}
	var result State
	if request.Mode == CustodyExitKeepLocal {
		localAnchor := custody.RecoveryLocalRef(run.ID)
		if err := custody.PreserveRecoveryAnchor(ctx, s.workDir(), localAnchor, divergence.local); err != nil {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_preserve_failed", fmt.Sprintf("the submitted local head could not be anchored before returning custody: %v; no files or branch refs were changed; custody was not recorded", err))
		}
		exit.Anchors = append(exit.Anchors, localAnchor)
		exit.KeptHead = divergence.local
		result = s.recoverKeepLocalAtCurrentHead(ctx, run, state, []string{run.ID}, []string{run.HeadSHA}, exit)
	} else {
		exit.KeptHead = divergence.preserved
		exit.Anchors = append(exit.Anchors, custody.RecoveryLocalRef(run.ID))
		if divergence.gateHead != divergence.preserved {
			exit.Anchors = append(exit.Anchors, custody.RecoveryGateRef(run.ID))
		}
		result = s.adoptTerminalDivergence(ctx, run, state, divergence, exit)
	}
	if result.Recovered {
		result.Recovery = divergence.evidence("acknowledged", request.Mode == CustodyExitKeepLocal)
	}
	return result
}

// anchorPreservedInWorktree fetches the preserved head from the gate's
// recovery ref and pins it at the same ref in the invoking worktree,
// create-only, so it never replaces conflicting evidence.
func (s *Service) anchorPreservedInWorktree(ctx context.Context, state State, d *terminalDivergence) (State, bool) {
	wd := s.workDir()
	if existing, err := git.Run(ctx, wd, "rev-parse", "--verify", d.anchorRef+"^{commit}"); err == nil && existing == d.preserved {
		return state, true
	}
	if err := git.FetchRemoteRef(ctx, wd, s.GateDir, d.anchorRef, d.preserved); err != nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_preserve_failed", "the preserved pipeline commits could not be fetched from the local gate; no files or refs were changed"), false
	}
	if err := custody.PreserveRecoveryAnchor(ctx, wd, d.anchorRef, d.preserved); err != nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_anchor_mismatch", "the invoking worktree recovery ref conflicts with the recorded pipeline head; inspect both objects before returning custody; no files or refs were changed"), false
	}
	return state, true
}

// adoptTerminalDivergence moves the gate branch, then the worktree, to the
// preserved head. The gate moves first so the next launch is not refused by a
// private mirror still at the submitted head (#163); its old head is anchored
// at refs/no-mistakes/recover-gate/<run> before the compare-and-swap, and the
// swap is reversed when the worktree move does not reach the preserved head.
// The worktree move is adoptPreservedHead's: the submitted head is anchored at
// refs/no-mistakes/recover-local/<run>, and the branch moves by an atomic
// compare-and-swap plus a read-tree that refuses to overwrite local changes.
func (s *Service) adoptTerminalDivergence(ctx context.Context, run *db.Run, state State, d *terminalDivergence, exit *db.CustodyExit) State {
	branchRef := "refs/heads/" + d.branch
	gateMoved := false
	if d.gateHead != d.preserved {
		if err := custody.PreserveRecoveryAnchor(ctx, s.GateDir, custody.RecoveryGateRef(run.ID), d.gateHead); err != nil {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_preserve_failed", "the gate branch head could not be anchored before adopting the preserved head; no files or branch refs were changed")
		}
		if _, err := git.Run(ctx, s.GateDir, "update-ref", branchRef, d.preserved, d.gateHead); err != nil {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_race", "the gate branch changed while custody was being returned; re-run the recovery; no files or branch refs were changed")
		}
		gateMoved = true
	}
	moved, ok := s.adoptPreservedHead(ctx, run, state, d.preserved, true)
	if !ok {
		return s.restorePreExit(ctx, d, gateMoved, moved)
	}
	result := s.finishRecover(ctx, run, true, exit)
	if result.Recovered {
		return result
	}
	return s.restorePreExit(ctx, d, gateMoved, result)
}

func (s *Service) restorePreExit(ctx context.Context, d *terminalDivergence, gateMoved bool, result State) State {
	rollbackCtx := context.WithoutCancel(ctx)
	branchRef := "refs/heads/" + d.branch
	localHead, err := git.Run(rollbackCtx, s.workDir(), "rev-parse", "--verify", branchRef)
	switch {
	case err != nil:
		result.Error += "; the local branch could not be inspected for restoration"
	case localHead == d.preserved:
		if _, err := git.Run(rollbackCtx, s.workDir(), "update-ref", branchRef, d.local, d.preserved); err != nil {
			result.Error += fmt.Sprintf("; the local branch could not be restored from %s to %s", d.preserved, d.local)
		} else if _, err := git.Run(rollbackCtx, s.workDir(), "read-tree", "-m", "-u", d.preserved, d.local); err != nil {
			result.Error += "; the local branch was restored but the worktree could not be restored"
		} else {
			result.Error += "; the local branch and worktree were restored to the submitted head"
		}
	case localHead == d.local:
		result.Error += "; the local branch is already at the submitted head; the worktree was left untouched"
	default:
		result.Error += fmt.Sprintf("; the local branch is at %s instead of %s; the branch and worktree were left untouched", localHead, d.preserved)
	}
	if gateMoved {
		if _, err := git.Run(rollbackCtx, s.GateDir, "update-ref", branchRef, d.gateHead, d.preserved); err != nil {
			result.Error += fmt.Sprintf("; the gate branch could not be restored from %s to %s", d.preserved, d.gateHead)
		} else {
			result.Error += "; the gate branch was restored to its exact pre-recovery head"
		}
	}
	fresh, _, _ := s.inspect(rollbackCtx)
	fresh.Safety = result.Safety
	fresh.Error = result.Error
	fresh.NextAction = nil
	fresh.Changed = fresh.Local.Head != d.local
	return fresh
}
