package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// cleanBaseFixture builds origin/main and a pushed feature branch that has
// diverged from it. mainContent and featureContent are written to the same
// file on each side, so equal content-free paths merge cleanly and the same
// path with different lines conflicts.
type cleanBaseFixture struct {
	dir, upstream, baseSHA, headSHA, mainTip string
}

func newCleanBaseFixture(t *testing.T, featureFile, mainFile string) cleanBaseFixture {
	t.Helper()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	writeFixtureFile(t, dir, "shared.txt", "base\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	writeFixtureFile(t, dir, featureFile, "feature change\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature change")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	gitCmd(t, dir, "checkout", "main")
	writeFixtureFile(t, dir, mainFile, "main moved\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "main moved")
	mainTip := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "feature")
	return cleanBaseFixture{dir: dir, upstream: upstream, baseSHA: baseSHA, headSHA: headSHA, mainTip: mainTip}
}

// A pull request merges from its own head, so a head that merges cleanly into
// a base that moved needs no rewrite: Rebase keeps it exactly (issue #237).
func TestRebaseStep_CleanHeadOnMovedBaseKeepsHeadWithoutRebase(t *testing.T) {
	t.Parallel()
	for _, strategy := range []string{config.RebaseStrategyRebase, config.RebaseStrategyMerge} {
		t.Run(strategy, func(t *testing.T) {
			t.Parallel()
			f := newCleanBaseFixture(t, "feature.txt", "main.txt")
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, f.dir, f.baseSHA, f.headSHA, config.Commands{})
			sctx.Run.Branch = "refs/heads/feature"
			sctx.Repo.UpstreamURL = f.upstream
			sctx.Config.Rebase.Strategy = strategy
			var logs []string
			sctx.Log = func(s string) { logs = append(logs, s) }

			outcome, err := (&RebaseStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.NeedsApproval || outcome.SkipRemaining {
				t.Fatalf("outcome = %+v, want a plain completion", outcome)
			}
			if head := gitCmd(t, f.dir, "rev-parse", "HEAD"); head != f.headSHA {
				t.Fatalf("HEAD = %s, want the submitted head %s kept without a rebase", head, f.headSHA)
			}
			if sctx.Run.HeadSHA != f.headSHA {
				t.Fatalf("run head = %s, want %s", sctx.Run.HeadSHA, f.headSHA)
			}
			if _, err := git.Run(sctx.Ctx, f.dir, "merge-base", "--is-ancestor", f.mainTip, "HEAD"); err == nil {
				t.Fatal("the moved base was integrated into a head that merges cleanly")
			}
			joined := strings.Join(logs, "\n")
			if !strings.Contains(joined, "merges cleanly into origin/main at "+shortSHA(f.mainTip)) {
				t.Fatalf("logs do not record the clean base:\n%s", joined)
			}
			if strings.Contains(joined, "rebasing onto origin/main") || strings.Contains(joined, "merging origin/main") {
				t.Fatalf("Rebase integrated a base the head merges cleanly into:\n%s", joined)
			}
		})
	}
}

// A real conflict is the one case that still integrates, so it still parks at
// the conflict gate with the head restored.
func TestRebaseStep_ConflictingHeadOnMovedBaseStillGates(t *testing.T) {
	t.Parallel()
	for _, strategy := range []string{config.RebaseStrategyRebase, config.RebaseStrategyMerge} {
		t.Run(strategy, func(t *testing.T) {
			t.Parallel()
			f := newCleanBaseFixture(t, "shared.txt", "shared.txt")
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, f.dir, f.baseSHA, f.headSHA, config.Commands{})
			sctx.Run.Branch = "refs/heads/feature"
			sctx.Repo.UpstreamURL = f.upstream
			sctx.Config.Rebase.Strategy = strategy

			outcome, err := (&RebaseStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if !outcome.NeedsApproval || !outcome.AutoFixable {
				t.Fatalf("outcome = %+v, want the auto-fixable conflict gate", outcome)
			}
			if !strings.Contains(outcome.Findings, "shared.txt") || !strings.Contains(outcome.Findings, "origin/main") {
				t.Fatalf("findings do not name the conflict: %s", outcome.Findings)
			}
			if head := gitCmd(t, f.dir, "rev-parse", "HEAD"); head != f.headSHA {
				t.Fatalf("HEAD = %s, want %s restored after the conflict", head, f.headSHA)
			}
			if status := gitStatusPorcelain(t, f.dir); status != "" {
				t.Fatalf("worktree left dirty after the conflict: %s", status)
			}
		})
	}
}

// A head strictly behind its base is not reset onto it: the head is kept and
// the empty proposed change ends the run as before.
func TestRebaseStep_HeadBehindBaseIsKeptRatherThanReset(t *testing.T) {
	t.Parallel()
	f := newCleanBaseFixture(t, "feature.txt", "main.txt")
	gitCmd(t, f.dir, "reset", "--hard", f.baseSHA)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, f.dir, f.baseSHA, f.baseSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/behind"
	sctx.Repo.UpstreamURL = f.upstream

	outcome, err := (&RebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if head := gitCmd(t, f.dir, "rev-parse", "HEAD"); head != f.baseSHA {
		t.Fatalf("HEAD = %s, want %s kept rather than reset onto the base", head, f.baseSHA)
	}
	if !outcome.SkipRemaining {
		t.Fatalf("outcome = %+v, want the empty proposed change to skip the remaining steps", outcome)
	}
}

// The pushed source branch is not the base: commits already on it must still
// be integrated, or publication would drop them.
func TestRebaseStep_SourceBranchCommitsStillIntegrateWhenBaseIsClean(t *testing.T) {
	t.Parallel()
	f := newCleanBaseFixture(t, "feature.txt", "main.txt")
	writeFixtureFile(t, f.dir, "remote-only.txt", "remote\n")
	gitCmd(t, f.dir, "add", "-A")
	gitCmd(t, f.dir, "commit", "-m", "remote-only commit")
	remoteTip := gitCmd(t, f.dir, "rev-parse", "HEAD")
	gitCmd(t, f.dir, "push", "origin", "feature")
	gitCmd(t, f.dir, "reset", "--hard", f.headSHA)

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, f.dir, f.baseSHA, f.headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = f.upstream

	if _, err := (&RebaseStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if head := gitCmd(t, f.dir, "rev-parse", "HEAD"); head != remoteTip {
		t.Fatalf("HEAD = %s, want the pushed source branch %s integrated", head, remoteTip)
	}
	if _, err := git.Run(sctx.Ctx, f.dir, "merge-base", "--is-ancestor", f.mainTip, "HEAD"); err == nil {
		t.Fatal("the clean moved base was integrated")
	}
}

// A change that already landed on the base through a squash merge leaves the
// head's commits outside the base, so merge-base..HEAD stays nonempty. The
// head is kept (issue #237), but its effective proposal is empty: merging it
// into the target reproduces the target tree exactly, so the run must end as
// the old rebase-then-empty-diff path did instead of reviewing and testing a
// change that already shipped. A partially landed change still validates.
func TestRebaseStep_AlreadySquashMergedHeadSkipsRemaining(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		extraFile bool
		wantSkip  bool
	}{
		{name: "fully landed", wantSkip: true},
		{name: "partly landed", extraFile: true, wantSkip: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCleanBaseFixture(t, "feature.txt", "main.txt")
			if tc.extraFile {
				writeFixtureFile(t, f.dir, "extra.txt", "not on main\n")
				gitCmd(t, f.dir, "add", "-A")
				gitCmd(t, f.dir, "commit", "-m", "extra feature change")
				f.headSHA = gitCmd(t, f.dir, "rev-parse", "HEAD")
				gitCmd(t, f.dir, "push", "origin", "feature")
			}
			// Squash-merge the original feature change onto main.
			gitCmd(t, f.dir, "checkout", "main")
			writeFixtureFile(t, f.dir, "feature.txt", "feature change\n")
			gitCmd(t, f.dir, "add", "-A")
			gitCmd(t, f.dir, "commit", "-m", "feature change (squash)")
			gitCmd(t, f.dir, "push", "origin", "main")
			gitCmd(t, f.dir, "checkout", "feature")

			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, f.dir, f.baseSHA, f.headSHA, config.Commands{})
			sctx.Run.Branch = "refs/heads/feature"
			sctx.Repo.UpstreamURL = f.upstream
			outcome, err := (&RebaseStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.SkipRemaining != tc.wantSkip || outcome.NeedsApproval {
				t.Fatalf("outcome = %+v, want SkipRemaining=%t", outcome, tc.wantSkip)
			}
			if head := gitCmd(t, f.dir, "rev-parse", "HEAD"); head != f.headSHA {
				t.Fatalf("HEAD = %s, want the submitted head %s kept", head, f.headSHA)
			}
		})
	}
}

// A run parked on a conflicting target advance resumes with a pending rewind
// pinned to the checked target. When that checked target already carries the
// head's change (squash-merged while the run was parked), the kept head
// proposes nothing and the run must end, judged against the pinned target
// rather than a tracking ref that may have moved since.
func TestUpdateHeadSHA_PinnedRewindEndsAlreadyLandedProposal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		extraFile bool
		wantSkip  bool
	}{
		{name: "fully landed", wantSkip: true},
		{name: "partly landed", extraFile: true, wantSkip: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCleanBaseFixture(t, "feature.txt", "main.txt")
			if tc.extraFile {
				writeFixtureFile(t, f.dir, "extra.txt", "not on main\n")
				gitCmd(t, f.dir, "add", "-A")
				gitCmd(t, f.dir, "commit", "-m", "extra feature change")
				f.headSHA = gitCmd(t, f.dir, "rev-parse", "HEAD")
			}
			gitCmd(t, f.dir, "checkout", "main")
			writeFixtureFile(t, f.dir, "feature.txt", "feature change\n")
			gitCmd(t, f.dir, "add", "-A")
			gitCmd(t, f.dir, "commit", "-m", "feature change (squash)")
			checked := gitCmd(t, f.dir, "rev-parse", "HEAD")
			gitCmd(t, f.dir, "checkout", "feature")
			// The tracking ref no longer names the checked target.
			gitCmd(t, f.dir, "update-ref", "refs/remotes/origin/main", f.mainTip)

			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, f.dir, f.baseSHA, f.headSHA, config.Commands{})
			outcome, err := updateHeadSHA(sctx.Ctx, sctx, "main", checked)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.SkipRemaining != tc.wantSkip {
				t.Fatalf("outcome = %+v, want SkipRemaining=%t", outcome, tc.wantSkip)
			}
		})
	}
}

func TestRebaseStep_ParkedConflictResumesOnSquashLandedTarget(t *testing.T) {
	for _, mode := range []string{"normal", "fix"} {
		for _, partlyLanded := range []bool{false, true} {
			name := mode + "/fully landed"
			if partlyLanded {
				name = mode + "/partly landed"
			}
			t.Run(name, func(t *testing.T) {
				f := newCleanBaseFixture(t, "shared.txt", "shared.txt")
				if partlyLanded {
					writeFixtureFile(t, f.dir, "extra.txt", "still proposed\n")
					gitCmd(t, f.dir, "add", "extra.txt")
					gitCmd(t, f.dir, "commit", "-m", "remaining proposal")
					f.headSHA = gitCmd(t, f.dir, "rev-parse", "HEAD")
					gitCmd(t, f.dir, "push", "origin", "feature")
				}
				gitCmd(t, f.dir, "push", "--force", "origin", f.baseSHA+":main")
				gitCmd(t, f.dir, "update-ref", "refs/remotes/origin/main", f.baseSHA)
				c := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, f.dir, f.baseSHA, f.headSHA, config.Commands{})
				c.Repo.UpstreamURL = f.upstream
				c.Config.Rebase.MaxTargetAdvanceRewinds = 3
				selected := pipeline.PRTargetSelection{TargetBranch: "main"}
				if _, err := guardPRContextWithSelection(c, types.StepRebase, selected, false); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, f.dir, "push", "origin", "main")
				decision, err := guardPRContextWithSelection(c, types.StepTest, selected, false)
				if err != nil || decision.RestartFrom != types.StepRebase {
					t.Fatalf("conflicting target must create a rewind: %+v, %v", decision, err)
				}
				step := &RebaseStep{}
				parked, err := step.Execute(c)
				if err != nil || parked == nil || !parked.NeedsApproval || !parked.AutoFixable {
					t.Fatalf("T1 must park at the conflict gate: %+v, %v", parked, err)
				}
				pending, err := c.DB.GetPendingTargetRewind(c.Run.ID)
				if err != nil || pending == nil || pending.TargetSHA != f.mainTip || pending.LocalHeadSHA != f.headSHA {
					t.Fatalf("park must retain T1 and H: %+v, %v", pending, err)
				}
				gitCmd(t, f.dir, "checkout", "main")
				writeFixtureFile(t, f.dir, "shared.txt", "feature change\n")
				gitCmd(t, f.dir, "add", "shared.txt")
				gitCmd(t, f.dir, "commit", "-m", "squash land feature change")
				target := gitCmd(t, f.dir, "rev-parse", "HEAD")
				gitCmd(t, f.dir, "push", "origin", "main")
				gitCmd(t, f.dir, "checkout", "feature")
				c.Fixing = true
				c.PreviousFindings = parked.Findings
				_, cleanup, err := checkedRewindTargets(c, rebaseTargets("feature", "main"))
				if err != nil {
					t.Fatal(err)
				}
				if err := cleanup(); err != nil {
					t.Fatal(err)
				}
				pending, err = c.DB.GetPendingTargetRewind(c.Run.ID)
				if err != nil || pending == nil || pending.TargetSHA != target || pending.LocalHeadSHA != f.headSHA || c.Fixing {
					t.Fatalf("refresh must bind T2 and H and clear the old fix selection: %+v, fixing=%t, %v", pending, c.Fixing, err)
				}
				c.Fixing = mode == "fix"
				outcome, err := step.Execute(c)
				if err != nil || outcome == nil || outcome.NeedsApproval || outcome.SkipRemaining != !partlyLanded {
					t.Fatalf("resumed proposal must skip only when fully landed: %+v, %v", outcome, err)
				}
				if c.Fixing != (mode == "fix") {
					t.Fatal("resume did not exercise the requested completion caller")
				}
				if mode == "fix" && outcome.FixSummary != NoChangesAppliedSummary {
					t.Fatalf("clean fix-mode completion = %q", outcome.FixSummary)
				}
				if head := gitCmd(t, f.dir, "rev-parse", "HEAD"); head != f.headSHA || c.Run.HeadSHA != f.headSHA {
					t.Fatalf("resume rewrote kept H: live=%s recorded=%s want=%s", head, c.Run.HeadSHA, f.headSHA)
				}
			})
		}
	}
}
