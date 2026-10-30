package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// newCLITerminalDivergenceFixture reproduces issue #233's never-published
// shape (controller#9028) for the CLI surface: a cancelled run whose
// conflict-resolved rebase is pinned only at the gate's recovery ref, while the
// clean registered worktree and the gate branch sit at the submitted head.
func newCLITerminalDivergenceFixture(t *testing.T) cliRecoverFixture {
	t.Helper()
	t.Setenv("NM_HOME", filepath.Join(t.TempDir(), "nm-home"))
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	cliGit(t, root, "init", "--bare", remote)
	local := filepath.Join(root, "operator")
	cliGit(t, root, "init", "-b", "main", local)
	cliGit(t, local, "config", "user.name", "Test")
	cliGit(t, local, "config", "user.email", "test@example.com")
	write := func(dir, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(local, "file.txt", "base\n")
	cliGit(t, local, "add", "file.txt")
	cliGit(t, local, "commit", "-m", "base")
	base := cliGit(t, local, "rev-parse", "HEAD")
	cliGit(t, local, "checkout", "-b", "feature/recover")
	write(local, "file.txt", "lane change\n")
	cliGit(t, local, "commit", "-am", "lane: change line one")
	submitted := cliGit(t, local, "rev-parse", "HEAD")
	cliGit(t, local, "checkout", "main")
	write(local, "file.txt", "upstream change\n")
	cliGit(t, local, "commit", "-am", "upstream change")
	cliGit(t, local, "checkout", "feature/recover")

	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	registeredRoot, err := git.FindGitRoot(local)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.InsertRepo(registeredRoot, remote, "main")
	if err != nil {
		t.Fatal(err)
	}
	gate := p.RepoDir(repo.ID)
	cliGit(t, filepath.Dir(gate), "init", "--bare", gate)
	cliGit(t, local, "push", gate, "refs/heads/main:refs/heads/main", "refs/heads/feature/recover:refs/heads/feature/recover")
	run, err := database.InsertRun(repo.ID, "feature/recover", submitted, base)
	if err != nil {
		t.Fatal(err)
	}

	pipeline := filepath.Join(root, "pipeline")
	cliGit(t, root, "-c", "core.autocrlf=false", "clone", gate, pipeline)
	cliGit(t, pipeline, "config", "user.name", "Pipeline")
	cliGit(t, pipeline, "config", "user.email", "pipeline@example.com")
	cliGit(t, pipeline, "checkout", "-b", "rebased", "origin/main")
	write(pipeline, "file.txt", "lane change over upstream\n")
	cliGit(t, pipeline, "commit", "-am", "lane: change line one")
	write(pipeline, "fix.txt", "review fix\n")
	cliGit(t, pipeline, "add", "fix.txt")
	cliGit(t, pipeline, "commit", "-m", "no-mistakes(review): fix")
	preserved := cliGit(t, pipeline, "rev-parse", "HEAD")
	cliGit(t, pipeline, "push", "origin", "HEAD:"+custody.RecoveryRef(run.ID))

	if err := database.UpdateRunHeadSHA(run.ID, preserved); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatusWithVerifiedHead(run.ID, types.RunCancelled, preserved); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	chdir(t, local)
	return cliRecoverFixture{
		local: local, gate: gate, remote: remote, base: base, submitted: submitted,
		preserved: preserved, runID: run.ID, repoID: repo.ID,
	}
}

func TestAxiSyncTerminalDivergenceAcknowledgedAdoptEndToEnd(t *testing.T) {
	f := newCLITerminalDivergenceFixture(t)
	adopt := "no-mistakes axi sync --recover --adopt-preserved --acknowledge " + f.submitted

	out, err := executeCmd("axi", "sync", "--check")
	var ee *exitError
	if err == nil || !asExitError(err, &ee) || ee.code != 1 {
		t.Fatalf("check should refuse with exit 1, got %#v\n%s", err, out)
	}
	for _, want := range []string{
		"safety: blocked_recover_unproven_divergence", "code: choose_custody_exit", "command: " + adopt,
		"source: terminal_divergence", "local_only", "preserved_only", f.submitted, "lane: change line one",
		"--keep-local --acknowledge " + f.preserved,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("check output missing %q:\n%s", want, out)
		}
	}

	// Unacknowledged keep-local no longer returns custody silently.
	out, err = executeCmd("axi", "sync", "--recover", "--keep-local")
	if err == nil || !asExitError(err, &ee) || ee.code != 1 || !strings.Contains(out, "safety: blocked_recover_unproven_divergence") {
		t.Fatalf("unacknowledged keep-local should refuse, got %#v\n%s", err, out)
	}
	out, err = executeCmd("axi", "sync", "--recover", "--adopt-preserved", "--acknowledge", f.preserved)
	if err == nil || !asExitError(err, &ee) || ee.code != 1 || !strings.Contains(out, "safety: blocked_recover_acknowledgement_mismatch") {
		t.Fatalf("mismatched acknowledgement should refuse, got %#v\n%s", err, out)
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatalf("refusals moved HEAD to %s", got)
	}

	t.Setenv("NO_MISTAKES_OWNER", "thread-233")
	out, err = executeCmd("axi", "sync", "--recover", "--adopt-preserved", "--acknowledge", f.submitted)
	if err != nil || !strings.Contains(out, "recovered: true") || !strings.Contains(out, "proof: acknowledged") {
		t.Fatalf("acknowledged adopt: %v\n%s", err, out)
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != f.preserved {
		t.Fatalf("HEAD = %s, want preserved %s", got, f.preserved)
	}
	if got := cliGit(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != f.preserved {
		t.Fatalf("gate branch = %s, want preserved %s", got, f.preserved)
	}
	if got := cliGit(t, f.local, "rev-parse", custody.RecoveryLocalRef(f.runID)); got != f.submitted {
		t.Fatalf("submitted head anchor = %s, want %s", got, f.submitted)
	}
	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	exit, err := database.GetRunCustodyExit(f.runID)
	if err != nil || exit == nil || exit.Mode != "adopt_preserved" || exit.Acknowledged != f.submitted || !strings.Contains(exit.Invoker, "thread-233") {
		t.Fatalf("custody exit record = %#v, %v", exit, err)
	}

	// Repeating the same acknowledged exit is an idempotent success.
	out, err = executeCmd("axi", "sync", "--recover", "--adopt-preserved", "--acknowledge", f.submitted)
	if err != nil || !strings.Contains(out, "recovered: true") {
		t.Fatalf("repeated acknowledged adopt: %v\n%s", err, out)
	}
}
