package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
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
