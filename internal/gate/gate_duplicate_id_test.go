package gate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

// This file holds the regression tests for issue #160: an init from a second
// clone (or an unregistered copy) of an already-registered repo derives the
// same deterministic repo ID, fails the insert with a UNIQUE constraint on
// repos.id, and its rollback then removed the EXISTING shared gate - its
// objects, its refs and its linked-worktree administration - while the
// database row and the run records stayed. The gate was simply gone.
//
// The shape the incident takes is a registration whose checkout was moved:
// init reattaches the gate and keeps the repo ID the ORIGINAL path derived,
// so a copy restored at that original path derives the same ID again. The
// refusal therefore has to be decided before anything is created, and a
// rollback may only remove what the failing attempt itself created.

// treeSnapshot records the exact content of every file under root, so a later
// comparison can prove that nothing was rewritten, added or removed. File
// contents are hashed rather than compared byte by byte so the snapshot stays
// readable in a failure message.
func treeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			out[rel] = "symlink:" + target
		case info.IsDir():
			out[rel] = "dir"
		case info.Mode().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			out[rel] = fmt.Sprintf("file:%s:%s", info.Mode().Perm(), hex.EncodeToString(sum[:]))
		default:
			out[rel] = "other:" + info.Mode().String()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return out
}

// compareSnapshots reports every difference between two snapshots of the same
// tree. A gate that lost a file, gained one or had one rewritten shows up
// here, which is exactly what the incident did to the shared gate.
func compareSnapshots(t *testing.T, label string, before, after map[string]string) {
	t.Helper()
	var diffs []string
	for path, want := range before {
		got, ok := after[path]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("%s: removed", path))
			continue
		}
		if got != want {
			diffs = append(diffs, fmt.Sprintf("%s: rewritten (%s -> %s)", path, want, got))
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			diffs = append(diffs, fmt.Sprintf("%s: added", path))
		}
	}
	sort.Strings(diffs)
	if len(diffs) > 0 {
		t.Errorf("%s changed in %d place(s):\n  %s", label, len(diffs), strings.Join(diffs, "\n  "))
	}
}

// gateRefSnapshot records every ref in the gate, so a lost sentinel ref (or a
// ref moved to another name) is visible.
func gateRefSnapshot(t *testing.T, bareDir string) string {
	t.Helper()
	out, err := exec.Command("git", "--git-dir", bareDir, "for-each-ref", "--format=%(refname) %(objectname)").CombinedOutput()
	if err != nil {
		t.Fatalf("list refs of %s: %v: %s", bareDir, err, out)
	}
	return string(out)
}

// seedGateSentinel gives the gate content of its own: a commit that exists
// only in the gate, a ref naming it, and a linked worktree carved from it.
// The linked worktree is what carries the worktree administration the
// incident destroyed, and it is what a later read-back has to find usable.
func seedGateSentinel(t *testing.T, bareDir string) (commit string, worktree string) {
	t.Helper()
	ctx := context.Background()

	treeOut, err := exec.Command("git", "--git-dir", bareDir, "mktree").Output()
	if err != nil {
		t.Fatalf("write gate tree: %v", err)
	}
	commitOut, err := exec.Command("git", "--git-dir", bareDir, "-c", "user.name=Test", "-c", "user.email=test@test.com",
		"commit-tree", strings.TrimSpace(string(treeOut)), "-m", "gate sentinel").Output()
	if err != nil {
		t.Fatalf("write gate commit: %v", err)
	}
	commit = strings.TrimSpace(string(commitOut))

	if out, err := exec.Command("git", "--git-dir", bareDir, "update-ref", "refs/gate-sentinel", commit).CombinedOutput(); err != nil {
		t.Fatalf("write gate sentinel ref: %v: %s", err, out)
	}

	worktree = filepath.Join(t.TempDir(), "gate-worktree")
	if err := gitpkg.WorktreeAdd(ctx, bareDir, worktree, commit); err != nil {
		t.Fatalf("add gate worktree: %v", err)
	}
	return commit, worktree
}

// assertGateWorktreeUsable proves the linked worktree of the gate still
// resolves to a repository. After the incident it failed with "gitfile does
// not point to a valid repository", which is the operator-visible symptom of
// a gate whose worktree administration was deleted.
func assertGateWorktreeUsable(t *testing.T, worktree, commit string) {
	t.Helper()
	out, err := exec.Command("git", "-C", worktree, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("linked worktree of the gate is unusable: %v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != commit {
		t.Errorf("linked worktree HEAD = %q, want %q", got, commit)
	}
}

// TestInitRefusesDuplicateRepoIDWithoutTouchingTheRegisteredGate is the
// regression test for issue #160.
//
// A registered checkout is moved, init reattaches its gate and keeps the repo
// ID the original path derived, and a copy is then restored at that original
// path. The copy derives the same repo ID, so the insert collides with the
// registration that owns the gate. The refusal must name the registered path
// and must leave the shared gate - its objects, its refs and its
// linked-worktree administration - exactly as it was.
func TestInitRefusesDuplicateRepoIDWithoutTouchingTheRegisteredGate(t *testing.T) {
	ctx := context.Background()
	workDir := setupTestRepo(t)
	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)

	registered, _, err := Init(ctx, d, p, workDir)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	gateDir := p.RepoDir(registered.ID)
	commit, gateWorktree := seedGateSentinel(t, gateDir)

	// The registration has run history of its own before anything else
	// happens, so the read-back below proves that history survived rather
	// than that a new run can be written.
	seededRun, err := d.InsertRun(registered.ID, "feature", "headsha", "basesha")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}

	// The checkout moves. Init from the new path reattaches the same gate
	// and keeps the repo ID the ORIGINAL path derived.
	moved := filepath.Join(filepath.Dir(workDir), "moved")
	if err := os.Rename(workDir, moved); err != nil {
		t.Fatalf("move working dir: %v", err)
	}
	reattached, created, err := Init(ctx, d, p, moved)
	if err != nil {
		t.Fatalf("init after move: %v", err)
	}
	if created {
		t.Fatal("init after a move must reattach the existing gate, not create one")
	}
	if reattached.ID != registered.ID {
		t.Fatalf("repo ID after move = %q, want %q", reattached.ID, registered.ID)
	}
	if reattached.WorkingPath != moved {
		t.Fatalf("working path after move = %q, want %q", reattached.WorkingPath, moved)
	}

	// A copy of the checkout is restored at the ORIGINAL path while the
	// moved one still exists. It derives the same repo ID, so the insert
	// collides with the registration that owns the gate.
	copyDirTree(t, moved, workDir)

	beforeTree := treeSnapshot(t, gateDir)
	beforeRefs := gateRefSnapshot(t, gateDir)

	_, _, err = Init(ctx, d, p, workDir)
	if err == nil {
		t.Fatal("init from a checkout that derives a registered repo ID must be refused")
	}
	if !strings.Contains(err.Error(), moved) {
		t.Errorf("the refusal does not name the registered path %q: %v", moved, err)
	}

	// The refusal must be typed, so a caller can tell it from any other
	// init failure.
	var dup *DuplicateRepoIDError
	if !errors.As(err, &dup) {
		t.Fatalf("init error = %v (%T), want a *gate.DuplicateRepoIDError", err, err)
	}
	if dup.ID != registered.ID {
		t.Errorf("duplicate error ID = %q, want %q", dup.ID, registered.ID)
	}
	if dup.RegisteredPath != moved {
		t.Errorf("duplicate error registered path = %q, want %q", dup.RegisteredPath, moved)
	}
	if dup.CallerPath != workDir {
		t.Errorf("duplicate error caller path = %q, want %q", dup.CallerPath, workDir)
	}
	if dup.GateDir != gateDir {
		t.Errorf("duplicate error gate dir = %q, want %q", dup.GateDir, gateDir)
	}

	// The shared gate must be byte-for-byte what it was.
	afterTree := treeSnapshot(t, gateDir)
	compareSnapshots(t, "shared gate", beforeTree, afterTree)
	if got := gateRefSnapshot(t, gateDir); got != beforeRefs {
		t.Errorf("shared gate refs changed:\nbefore:\n%s\nafter:\n%s", beforeRefs, got)
	}
	assertGateWorktreeUsable(t, gateWorktree, commit)

	// The registration and its run history must survive.
	dbRepo, err := d.GetRepo(registered.ID)
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}
	if dbRepo == nil {
		t.Fatal("the registered repo row was destroyed by the refused init")
	}
	if dbRepo.WorkingPath != moved {
		t.Errorf("registered working path = %q, want %q", dbRepo.WorkingPath, moved)
	}
	runs, err := d.GetRunsByRepo(registered.ID)
	if err != nil {
		t.Fatalf("get runs: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != seededRun.ID {
		t.Errorf("runs after the refused init = %+v, want only the seeded run %q", runs, seededRun.ID)
	}

	// The refused checkout must not have been registered either.
	if copy, err := d.GetRepoByPath(workDir); err != nil {
		t.Fatalf("get repo by path: %v", err)
	} else if copy != nil {
		t.Errorf("the refused checkout was registered as %+v", copy)
	}
}

// TestInitCopyAtAnotherPathLeavesTheRegisteredGateUntouched covers the
// successful half of the same question: an init that succeeds must leave the
// other registration's gate exactly as it was, because the two gates are
// different directories and only one of them is this init's to write.
func TestInitCopyAtAnotherPathLeavesTheRegisteredGateUntouched(t *testing.T) {
	ctx := context.Background()
	workDir := setupTestRepo(t)
	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)

	registered, _, err := Init(ctx, d, p, workDir)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	gateDir := p.RepoDir(registered.ID)
	commit, gateWorktree := seedGateSentinel(t, gateDir)

	copyDir := filepath.Join(filepath.Dir(workDir), "copy")
	copyDirTree(t, workDir, copyDir)

	beforeTree := treeSnapshot(t, gateDir)
	beforeRefs := gateRefSnapshot(t, gateDir)

	copied, created, err := Init(ctx, d, p, copyDir)
	if err != nil {
		t.Fatalf("init on the copy: %v", err)
	}
	if !created {
		t.Error("init on the copy must create its own gate")
	}
	if copied.ID == registered.ID {
		t.Fatal("the copy must not share the registered repo ID")
	}
	if copied.ID != repoID(copyDir) {
		t.Errorf("copy repo ID = %q, want %q", copied.ID, repoID(copyDir))
	}

	afterTree := treeSnapshot(t, gateDir)
	compareSnapshots(t, "shared gate", beforeTree, afterTree)
	if got := gateRefSnapshot(t, gateDir); got != beforeRefs {
		t.Errorf("shared gate refs changed:\nbefore:\n%s\nafter:\n%s", beforeRefs, got)
	}
	assertGateWorktreeUsable(t, gateWorktree, commit)
}

// TestInitProvisionFailureRemovesOnlyWhatThisAttemptCreated injects a
// provisioning failure and checks the two halves of the rollback rule: a gate
// directory this attempt created is removed again, and a gate directory that
// was already there is left alone.
func TestInitProvisionFailureRemovesOnlyWhatThisAttemptCreated(t *testing.T) {
	ctx := context.Background()
	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)

	// Another registration's gate, which nothing here may touch.
	otherWork := setupTestRepo(t)
	other, _, err := Init(ctx, d, p, otherWork)
	if err != nil {
		t.Fatalf("init other: %v", err)
	}
	otherGate := p.RepoDir(other.ID)
	otherCommit, otherWorktree := seedGateSentinel(t, otherGate)
	otherBefore := treeSnapshot(t, otherGate)

	// This checkout's own gate, already there although its registration is
	// gone (the registry was lost): its gate remote points at it, so init
	// adopts and repairs it - and a failed repair must not remove it.
	ownWork := setupTestRepo(t)
	ownGate := p.RepoDir(repoID(ownWork))
	initBareAt(t, ownGate)
	if err := gitpkg.AddRemote(ctx, ownWork, RemoteName, ownGate); err != nil {
		t.Fatalf("wire own gate: %v", err)
	}

	// A checkout with no gate directory at all: whatever directory appears
	// belongs to the failing attempt alone.
	freshWork := setupTestRepo(t)
	freshGate := p.RepoDir(repoID(freshWork))

	oldEnsure := ensureGateHooksPathIsolation
	ensureGateHooksPathIsolation = func(context.Context, string) (bool, error) {
		return false, errors.New("injected provisioning failure")
	}
	t.Cleanup(func() { ensureGateHooksPathIsolation = oldEnsure })

	for _, work := range []string{ownWork, freshWork} {
		if _, _, err := Init(ctx, d, p, work); err == nil || !strings.Contains(err.Error(), "injected provisioning failure") {
			t.Fatalf("init at %s must fail with the injected provisioning failure, got %v", work, err)
		}
	}

	// The gate this attempt created is gone again, and so is the remote it
	// added.
	if _, err := os.Lstat(freshGate); !os.IsNotExist(err) {
		t.Errorf("the gate directory this attempt created still exists at %s", freshGate)
	}
	if has, err := gitpkg.HasRemote(ctx, freshWork, RemoteName); err != nil || has {
		t.Errorf("the failed init left its gate remote behind (has=%v, err=%v)", has, err)
	}
	// The gate that was already there survives, still wired.
	if !dirExists(ownGate) {
		t.Errorf("a pre-existing gate directory was removed by a failed init: %s", ownGate)
	}
	if url, err := gitpkg.GetConfiguredRemoteURL(ctx, ownWork, RemoteName); err != nil || url != ownGate {
		t.Errorf("own gate remote = %q, %v; want %q", url, err, ownGate)
	}
	// The other registration's gate is untouched.
	otherAfter := treeSnapshot(t, otherGate)
	compareSnapshots(t, "other gate", otherBefore, otherAfter)
	assertGateWorktreeUsable(t, otherWorktree, otherCommit)
}

// TestInitInsertFailureRemovesOnlyWhatThisAttemptCreated injects the
// registration failure the duplicate ID produced (the UNIQUE constraint on
// repos.id) and checks the same rollback rule on the other path: the gate
// directory this attempt created goes, the one that was already there stays.
//
// This is the failure that destroyed the shared gate in issue #160: the
// insert failed, and the rollback removed a gate directory this attempt had
// not created.
func TestInitInsertFailureRemovesOnlyWhatThisAttemptCreated(t *testing.T) {
	ctx := context.Background()
	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)

	oldInsert := insertRepoRecord
	insertRepoRecord = func(*db.DB, string, string, string, string, string) (*db.Repo, error) {
		return nil, errors.New("insert repo: constraint failed: UNIQUE constraint failed: repos.id (1555)")
	}
	t.Cleanup(func() { insertRepoRecord = oldInsert })

	// A checkout whose own gate directory already exists (and is wired).
	ownWork := setupTestRepo(t)
	ownGate := p.RepoDir(repoID(ownWork))
	initBareAt(t, ownGate)
	if err := gitpkg.AddRemote(ctx, ownWork, RemoteName, ownGate); err != nil {
		t.Fatalf("wire own gate: %v", err)
	}

	// A checkout with no gate directory at all.
	freshWork := setupTestRepo(t)
	freshGate := p.RepoDir(repoID(freshWork))

	if _, _, err := Init(ctx, d, p, ownWork); err == nil {
		t.Fatal("init must fail with the injected registration failure")
	}
	if !dirExists(ownGate) {
		t.Errorf("a pre-existing gate directory was removed by a failed init: %s", ownGate)
	}
	if url, err := gitpkg.GetConfiguredRemoteURL(ctx, ownWork, RemoteName); err != nil || url != ownGate {
		t.Errorf("own gate remote = %q, %v; want %q", url, err, ownGate)
	}

	if _, _, err := Init(ctx, d, p, freshWork); err == nil {
		t.Fatal("init must fail with the injected registration failure")
	}
	if _, err := os.Lstat(freshGate); !os.IsNotExist(err) {
		t.Errorf("the gate directory this attempt created still exists at %s", freshGate)
	}
	// The remote this attempt added is gone again.
	if _, err := gitpkg.GetRemoteURL(ctx, freshWork, RemoteName); err == nil {
		t.Error("the failed init left its gate remote behind")
	}
}

// TestInitInsertFailureLeavesAGateAnotherRegistrationOwns covers the race
// the rollback has to survive: a concurrent init won the registration for the
// same repo ID between this attempt's ownership check and its insert. The
// directory is then the winner's, and the loser must not remove it.
func TestInitInsertFailureLeavesAGateAnotherRegistrationOwns(t *testing.T) {
	ctx := context.Background()
	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)

	workDir := setupTestRepo(t)
	gateDir := p.RepoDir(repoID(workDir))

	oldInsert := insertRepoRecord
	insertRepoRecord = func(database *db.DB, id, workingPath, upstreamURL, forkURL, branch string) (*db.Repo, error) {
		// The winner of the race registers the same ID first.
		if _, err := database.InsertRepoWithIDAndFork(id, filepath.Join(filepath.Dir(workingPath), "winner"), upstreamURL, forkURL, branch); err != nil {
			t.Fatalf("insert winning registration: %v", err)
		}
		return nil, errors.New("insert repo: constraint failed: UNIQUE constraint failed: repos.id (1555)")
	}
	t.Cleanup(func() { insertRepoRecord = oldInsert })

	if _, _, err := Init(ctx, d, p, workDir); err == nil {
		t.Fatal("init must fail with the injected registration failure")
	}
	if _, err := os.Stat(gateDir); err != nil {
		t.Errorf("the gate of the registration that won the race was removed by the loser: %v", err)
	}
}

// initBareAt creates a bare repository at dir, outside of Init.
func initBareAt(t *testing.T, dir string) {
	t.Helper()
	if out, err := exec.Command("git", "init", "--bare", dir).CombinedOutput(); err != nil {
		t.Fatalf("init bare %s: %v: %s", dir, err, out)
	}
}

// newInitFixture returns a no-mistakes root and its registry.
func newInitFixture(t *testing.T) (*paths.Paths, *db.DB) {
	t.Helper()
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	return p, openTestDB(t, p)
}

// TestInitRefusesAnUnregisteredGateItIsNotWiredTo covers a fresh init whose
// gate path is already taken although no registration owns the ID - a gate
// left behind by an interrupted init or a removed registration - while the
// checkout's gate remote does not point at it. Provisioning it would rewrite
// its configuration and hooks, so the init must stop before touching it, with
// a typed error naming the path, and leave it byte-for-byte as it was.
func TestInitRefusesAnUnregisteredGateItIsNotWiredTo(t *testing.T) {
	ctx := context.Background()
	p, d := newInitFixture(t)

	work := setupTestRepo(t)
	gateDir := p.RepoDir(repoID(work))
	initBareAt(t, gateDir)
	commit, gateWorktree := seedGateSentinel(t, gateDir)
	before := treeSnapshot(t, gateDir)
	beforeRefs := gateRefSnapshot(t, gateDir)

	_, _, err := Init(ctx, d, p, work)
	var unreg *UnregisteredGateError
	if !errors.As(err, &unreg) {
		t.Fatalf("init error = %v (%T), want a *gate.UnregisteredGateError", err, err)
	}
	if unreg.GateDir != gateDir || unreg.CallerPath != work || unreg.ID != repoID(work) {
		t.Errorf("unregistered gate error = %+v", unreg)
	}
	if !strings.Contains(err.Error(), gateDir) {
		t.Errorf("the refusal does not name the gate path %q: %v", gateDir, err)
	}

	compareSnapshots(t, "unregistered gate", before, treeSnapshot(t, gateDir))
	if got := gateRefSnapshot(t, gateDir); got != beforeRefs {
		t.Errorf("unregistered gate refs changed:\nbefore:\n%s\nafter:\n%s", beforeRefs, got)
	}
	assertGateWorktreeUsable(t, gateWorktree, commit)
	if has, err := gitpkg.HasRemote(ctx, work, RemoteName); err != nil || has {
		t.Errorf("the refused init wired the checkout to the gate (has=%v, err=%v)", has, err)
	}
	if repo, err := d.GetRepoByPath(work); err != nil || repo != nil {
		t.Errorf("the refused checkout was registered: %+v, %v", repo, err)
	}
}

// TestInitAdoptsItsOwnUnregisteredGate keeps the recovery path open: a
// checkout whose gate remote already points at the gate (the registry was
// lost, the gate was not) re-registers that gate, history and all, instead of
// being refused.
func TestInitAdoptsItsOwnUnregisteredGate(t *testing.T) {
	ctx := context.Background()
	p, d := newInitFixture(t)

	work := setupTestRepo(t)
	first, _, err := Init(ctx, d, p, work)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	gateDir := p.RepoDir(first.ID)
	commit, gateWorktree := seedGateSentinel(t, gateDir)
	if err := d.DeleteRepo(first.ID); err != nil {
		t.Fatalf("drop registration: %v", err)
	}

	repo, created, err := Init(ctx, d, p, work)
	if err != nil {
		t.Fatalf("init of a checkout wired to its own unregistered gate: %v", err)
	}
	// The gate was already there, so this call did not create it: created
	// must be false, or the CLI's daemon-failure cleanup would eject (delete)
	// a gate it does not own.
	if created || repo.ID != first.ID {
		t.Errorf("re-registration = %+v (created=%v), want ID %q, not created", repo, created, first.ID)
	}
	if !dirExists(gateDir) {
		t.Fatalf("the adopted gate directory %s is gone", gateDir)
	}
	assertGateWorktreeUsable(t, gateWorktree, commit)
	if !strings.Contains(gateRefSnapshot(t, gateDir), "refs/gate-sentinel "+commit) {
		t.Error("the adopted gate lost its refs")
	}
}

// TestInitLeavesANonDirectoryAtTheGatePathAlone covers a gate path taken by
// something that is not a directory. This attempt did not create it, so a
// failed init must neither delete nor rewrite it.
func TestInitLeavesANonDirectoryAtTheGatePathAlone(t *testing.T) {
	ctx := context.Background()
	p, d := newInitFixture(t)

	work := setupTestRepo(t)
	gatePath := p.RepoDir(repoID(work))
	if err := os.WriteFile(gatePath, []byte("not a gate\n"), 0o644); err != nil {
		t.Fatalf("write file at gate path: %v", err)
	}
	// Wire the checkout to it, so not even a remote pointing at the path
	// makes a file a gate this init may provision.
	if err := gitpkg.AddRemote(ctx, work, RemoteName, gatePath); err != nil {
		t.Fatalf("wire remote: %v", err)
	}

	_, _, err := Init(ctx, d, p, work)
	var unreg *UnregisteredGateError
	if !errors.As(err, &unreg) {
		t.Fatalf("init error = %v (%T), want a *gate.UnregisteredGateError", err, err)
	}
	data, readErr := os.ReadFile(gatePath)
	if readErr != nil || string(data) != "not a gate\n" {
		t.Errorf("the file at the gate path was changed: %q, %v", data, readErr)
	}
}

// TestInitFailureKeepsAnAliasedGateRemoteLiteral covers the checkout half of
// the rollback rule: a failed init that did not change the gate remote must
// not write it. A remote configured through url.<base>.insteadOf reads back
// expanded, and "restoring" that expanded URL would replace the operator's
// alias for good.
func TestInitFailureKeepsAnAliasedGateRemoteLiteral(t *testing.T) {
	ctx := context.Background()
	p, d := newInitFixture(t)

	work := setupTestRepo(t)
	gateDir := p.RepoDir(repoID(work))
	initBareAt(t, gateDir)
	alias := "nmgate:" + filepath.Base(gateDir)
	if _, err := gitpkg.Run(ctx, work, "config", "url."+p.ReposDir()+"/.insteadOf", "nmgate:"); err != nil {
		t.Fatalf("configure alias: %v", err)
	}
	if err := gitpkg.AddRemote(ctx, work, RemoteName, alias); err != nil {
		t.Fatalf("add aliased remote: %v", err)
	}
	if url, err := gitpkg.GetRemoteURL(ctx, work, RemoteName); err != nil || url != gateDir {
		t.Fatalf("aliased remote resolves to %q, %v; want %q", url, err, gateDir)
	}

	oldEnsure := ensureGateHooksPathIsolation
	ensureGateHooksPathIsolation = func(context.Context, string) (bool, error) {
		return false, errors.New("injected provisioning failure")
	}
	t.Cleanup(func() { ensureGateHooksPathIsolation = oldEnsure })

	if _, _, err := Init(ctx, d, p, work); err == nil {
		t.Fatal("init must fail with the injected provisioning failure")
	}
	if url, err := gitpkg.GetConfiguredRemoteURL(ctx, work, RemoteName); err != nil || url != alias {
		t.Errorf("literal gate remote after a failed init = %q, %v; want the alias %q", url, err, alias)
	}
	if !dirExists(gateDir) {
		t.Error("the gate the checkout was wired to was removed by a failed init")
	}
}

// TestInitWaitsForTheInitLock proves the ownership decision is exclusive: an
// init cannot read the registry, create a gate or register while another
// init (or eject) holds the lock.
func TestInitWaitsForTheInitLock(t *testing.T) {
	ctx := context.Background()
	p, d := newInitFixture(t)
	work := setupTestRepo(t)
	gateDir := p.RepoDir(repoID(work))

	unlock, err := acquireInitLock(ctx, p)
	if err != nil {
		t.Fatalf("take init lock: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := Init(ctx, d, p, work)
		done <- err
	}()

	select {
	case err := <-done:
		unlock()
		t.Fatalf("init finished while another holder had the init lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := os.Lstat(gateDir); !os.IsNotExist(err) {
		t.Errorf("init created the gate while another holder had the init lock")
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatalf("init after the lock was released: %v", err)
	}
}

// TestInitGivesUpOnAHeldInitLock bounds the wait, so a stuck holder produces
// an error rather than a hang.
func TestInitGivesUpOnAHeldInitLock(t *testing.T) {
	ctx := context.Background()
	p, d := newInitFixture(t)
	work := setupTestRepo(t)

	oldWait := initLockWait
	initLockWait = 100 * time.Millisecond
	t.Cleanup(func() { initLockWait = oldWait })

	unlock, err := acquireInitLock(ctx, p)
	if err != nil {
		t.Fatalf("take init lock: %v", err)
	}
	defer unlock()

	if _, _, err := Init(ctx, d, p, work); err == nil || !strings.Contains(err.Error(), "init.lock") {
		t.Fatalf("init under a held lock = %v, want a lock timeout naming the lock", err)
	}
	if _, err := os.Lstat(p.RepoDir(repoID(work))); !os.IsNotExist(err) {
		t.Error("init created the gate without the init lock")
	}
}

// TestConcurrentInitsOfOneCheckoutShareOneGate races several inits of the
// same checkout. Exactly one creates and registers the gate, the others
// refresh it, and the gate survives with its content.
func TestConcurrentInitsOfOneCheckoutShareOneGate(t *testing.T) {
	ctx := context.Background()
	p, d := newInitFixture(t)
	work := setupTestRepo(t)

	const n = 4
	type result struct {
		repo    *db.Repo
		created bool
		err     error
	}
	results := make(chan result, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			<-start
			repo, created, err := Init(ctx, d, p, work)
			results <- result{repo, created, err}
		}()
	}
	close(start)

	created := 0
	var id string
	for i := 0; i < n; i++ {
		r := <-results
		if r.err != nil {
			t.Fatalf("concurrent init: %v", r.err)
		}
		if r.created {
			created++
		}
		if id == "" {
			id = r.repo.ID
		} else if r.repo.ID != id {
			t.Errorf("concurrent inits registered different IDs: %q and %q", id, r.repo.ID)
		}
	}
	if created != 1 {
		t.Errorf("concurrent inits created the gate %d times, want exactly once", created)
	}
	gateDir := p.RepoDir(id)
	if out, err := exec.Command("git", "--git-dir", gateDir, "rev-parse", "--is-bare-repository").CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "true" {
		t.Errorf("the shared gate is not a valid bare repository after concurrent inits: %v: %s", err, out)
	}
	if url, err := gitpkg.GetRemoteURL(ctx, work, RemoteName); err != nil || url != gateDir {
		t.Errorf("gate remote after concurrent inits = %q, %v; want %q", url, err, gateDir)
	}
}
