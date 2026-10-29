package gate

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gatecontext"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/procreap"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/worktrees"
)

var ensureGateHooksPathIsolation = git.EnsureHooksPathIsolation

var sweepRunWorktrees = procreap.SweepRunWorktrees

// insertRepoRecord writes the registration row of a fresh gate. It is a seam
// so a test can inject the failure a duplicate repo ID produces (the UNIQUE
// constraint on repos.id) without racing two real registrations against each
// other.
var insertRepoRecord = func(d *db.DB, id, workingPath, upstreamURL, forkURL, branch string) (*db.Repo, error) {
	return d.InsertRepoWithIDAndFork(id, workingPath, upstreamURL, forkURL, branch)
}

// RemoteName is the name of the git remote that points to the local gate.
const RemoteName = "no-mistakes"

// DuplicateRepoIDError reports that the deterministic repo ID a checkout
// derives is already owned by another registered checkout.
//
// The ID is derived from the absolute path of the checkout, so a copy
// restored at the path a registration was moved away from derives the very
// ID that registration still holds - and that ID names the shared gate
// directory holding the other checkout's objects, refs and linked-worktree
// administration. Provisioning it would write into that gate, and a rollback
// would delete it (issue #160).
//
// The error names the registered path because that is where the operator can
// act: the gate belongs to the registered checkout, and a checkout that wants
// a gate of its own needs a path of its own.
type DuplicateRepoIDError struct {
	// ID is the deterministic repo ID both checkouts derive.
	ID string
	// CallerPath is the checkout that asked to be initialized.
	CallerPath string
	// RegisteredPath is the checkout the ID already belongs to.
	RegisteredPath string
	// GateDir is the shared gate directory the ID names.
	GateDir string
}

func (e *DuplicateRepoIDError) Error() string {
	return fmt.Sprintf(
		"repo ID %s is already registered for %s; %s is a different checkout and cannot use that gate (%s)\n\n"+
			"Run no-mistakes init from the registered checkout, or move this checkout to a path of its own.",
		e.ID, e.RegisteredPath, e.CallerPath, e.GateDir)
}

// UnregisteredGateError reports that the gate path a fresh init would use is
// already taken although no registration owns its repo ID, and that the
// checkout's gate remote does not point at it.
//
// Whatever is there - a gate left behind by an interrupted init or a removed
// registration, or something the operator put there - is not this init's to
// rewrite or delete, so the init stops before touching it.
type UnregisteredGateError struct {
	// ID is the repo ID the checkout derives.
	ID string
	// CallerPath is the checkout that asked to be initialized.
	CallerPath string
	// GateDir is the gate path that is already taken.
	GateDir string
}

func (e *UnregisteredGateError) Error() string {
	return fmt.Sprintf(
		"gate path %s for repo ID %s already exists but no registered checkout owns it, and %s is not wired to it\n\n"+
			"Inspect it and move it away if it is not needed, then re-run no-mistakes init.",
		e.GateDir, e.ID, e.CallerPath)
}

// repoID generates a deterministic 12-char hex ID from an absolute path.
func repoID(absPath string) string {
	h := sha256.Sum256([]byte(absPath))
	return fmt.Sprintf("%x", h[:6])
}

// Init sets up a no-mistakes gate for the git repo at workDir.
// It creates a bare repo, installs the post-receive hook, best-effort
// isolates the bare repo's hooks path from shared local config writes when
// Git supports config --worktree, adds the no-mistakes remote, and records
// the repo in the database.
//
// Init is idempotent: re-running it on an already-initialized repo repairs and
// refreshes the gate (for example installing a newer hook, picking up hook-path
// isolation, or restoring a missing remote) instead of failing. This includes
// a working directory that was renamed or moved since the gate was created:
// the gate identified by the leftover no-mistakes remote is reattached at the
// new path, preserving its run history. The returned bool reports whether this
// call created a new gate directory (true) or registered or refreshed a gate
// that already existed (false). Callers may only roll back (eject) a gate when
// it is true: a gate that was already there is never this call's to delete.
func Init(ctx context.Context, d *db.DB, p *paths.Paths, workDir string) (*db.Repo, bool, error) {
	return InitWithFork(ctx, d, p, workDir, "")
}

// InitWithFork is Init plus an optional GitHub fork push URL. The origin remote
// remains the parent repository used for PRs. When forkURL is empty, an
// existing fork setting is preserved across idempotent refreshes.
func InitWithFork(ctx context.Context, d *db.DB, p *paths.Paths, workDir, forkURL string) (_ *db.Repo, _ bool, initErr error) {
	if classified, err := (gatecontext.Inspector{DB: d, Paths: p}).Inspect(ctx, gatecontext.Request{CWD: workDir, MarkerPresent: gatecontext.MarkerPresent()}); err != nil {
		return nil, false, err
	} else if classified.Nested {
		return nil, false, fmt.Errorf("%s", gatecontext.RefusalMessage(classified))
	}
	forkURL = strings.TrimSpace(forkURL)

	// Normalize worktrees back to the main repo root so one repo record works
	// from either the main checkout or any attached worktree.
	gitRoot, err := git.FindMainRepoRoot(workDir)
	if err != nil {
		return nil, false, fmt.Errorf("find git root: %w", err)
	}
	absRoot := gitRoot

	// Hold the init lock from the first registry read until this attempt has
	// registered or rolled back, so the ownership decided below cannot change
	// under it (issue #160).
	unlock, err := AcquireInitLock(ctx, p)
	if err != nil {
		return nil, false, err
	}
	defer unlock()

	// Look up any existing gate so we know whether this is a fresh init or a
	// refresh, and so we never tear down a working gate on a repair failure.
	existing, err := d.GetRepoByPath(absRoot)
	if err != nil {
		return nil, false, fmt.Errorf("check existing: %w", err)
	}
	if existing == nil {
		// No record at this path, but the repo may have been moved or renamed
		// after init; if so, reattach its existing gate instead of failing on
		// the leftover remote.
		existing, err = reattachRelocatedRepo(ctx, d, p, absRoot)
		if err != nil {
			return nil, false, err
		}
	}

	// Read origin URL. Keep the historical rewritten value for non-fork repos,
	// but preserve the literal parent URL when fork routing is configured.
	getOriginURL := git.GetRemoteURL
	if forkURL != "" || (existing != nil && strings.TrimSpace(existing.ForkURL) != "") {
		getOriginURL = git.GetConfiguredRemoteURL
	}
	upstreamURL, err := getOriginURL(ctx, absRoot, "origin")
	if err != nil {
		// A missing "origin" is a normal state for a fresh `git init` repo, so
		// give an actionable message instead of leaking git plumbing. Only
		// substitute it when origin is genuinely absent; any other git failure
		// keeps its original error.
		hasOrigin, listErr := git.HasRemote(ctx, absRoot, "origin")
		if listErr == nil && !hasOrigin {
			return nil, false, fmt.Errorf(
				"no 'origin' remote in %s\n\n"+
					"no-mistakes pushes your branch and opens a pull request, so it needs a remote to push to.\n"+
					"Add one, then re-run:\n\n"+
					"  git remote add origin <url>",
				absRoot)
		}
		return nil, false, fmt.Errorf("get origin url: %w", err)
	}
	if forkURL != "" {
		if err := validateForkRouting(ctx, upstreamURL, forkURL); err != nil {
			return nil, false, err
		}
	} else if existing == nil || strings.TrimSpace(existing.ForkURL) == "" {
		if err := refuseForkOriginMisrouting(ctx, absRoot); err != nil {
			return nil, false, err
		}
	}

	// Redact embedded credentials for everything that is persisted, logged, or
	// surfaced to the user. The bare gate keeps the full credentialled URL on
	// its "origin" remote via provisionGate below so worktrees carved from it
	// can still authenticate pushes; the push step resolves that credential
	// from the worktree at run time instead of trusting the DB copy.
	redactedUpstreamURL := safeurl.Redact(upstreamURL)

	id := repoID(absRoot)
	if existing != nil {
		id = existing.ID
	}
	bareDir := p.RepoDir(id)

	// A deterministic ID can already belong to another registration: the
	// caller is a copy restored at the path a registration was moved away
	// from, or two checkouts race for the same path. Decide that before
	// anything is created, so a refusal cannot write into the other
	// registration's shared gate - and so the rollback below has nothing
	// of theirs to remove (issue #160).
	if existing == nil {
		owner, err := d.GetRepo(id)
		if err != nil {
			return nil, false, fmt.Errorf("check repo id ownership: %w", err)
		}
		if owner != nil {
			return nil, false, &DuplicateRepoIDError{
				ID:             id,
				CallerPath:     absRoot,
				RegisteredPath: owner.WorkingPath,
				GateDir:        bareDir,
			}
		}
	}

	// The gate directory is this attempt's to remove only when this attempt
	// created it. Anything already at that path - a directory, a file, a
	// symlink - belongs to another registration, to an earlier interrupted
	// attempt or to the operator.
	gatePresent, err := pathPresent(bareDir)
	if err != nil {
		return nil, false, fmt.Errorf("inspect gate directory: %w", err)
	}
	if gatePresent && existing == nil {
		// No registration owns the ID, yet its gate path is taken. Adopt it
		// only when it is this checkout's own gate - a directory its gate
		// remote already points at, as after the registry was lost. Anything
		// else is not this init's to provision, because provisioning rewrites
		// the gate's configuration and hooks.
		if !dirExists(bareDir) || !remotePointsAt(ctx, absRoot, bareDir) {
			return nil, false, &UnregisteredGateError{ID: id, CallerPath: absRoot, GateDir: bareDir}
		}
	}

	// The gate remote of the working repo is restored to what it was when
	// this attempt fails, so a failed init leaves the caller's checkout as
	// it found it.
	priorRemote, err := snapshotWorkingRemote(ctx, absRoot)
	if err != nil {
		return nil, false, fmt.Errorf("snapshot checkout remote: %w", err)
	}

	// Existing gates are validated without mutation. Keep a journal of only
	// the metadata configureGate owns; never snapshot/restore objects, refs,
	// or worktree administration that other gate users may still be writing.
	provision := provisionGate
	if gatePresent {
		if err := git.ValidateBareRepository(ctx, bareDir); err != nil {
			return nil, false, err
		}
		metadata, err := snapshotInitMetadata(bareDir)
		if err != nil {
			return nil, false, fmt.Errorf("snapshot gate metadata: %w", err)
		}
		defer func() {
			if initErr != nil {
				initErr = errors.Join(initErr, metadata.restore())
			}
		}()
		provision = configureGate
	}

	gateDirCreated := false
	defer func() {
		if initErr == nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		initErr = errors.Join(initErr,
			rollbackWorkingRemote(cleanupCtx, absRoot, priorRemote),
			rollbackGate(d, bareDir, id, gateDirCreated))
	}()
	if !gatePresent {
		// Create the directory here, exclusively, so "this attempt created
		// it" is a fact rather than an inference.
		if err := os.MkdirAll(filepath.Dir(bareDir), 0o755); err != nil {
			return nil, false, fmt.Errorf("create repos directory: %w", err)
		}
		if err := os.Mkdir(bareDir, 0o777); err != nil {
			return nil, false, fmt.Errorf("create gate directory: %w", err)
		}
		gateDirCreated = true
	}

	// Provision (or repair) the on-disk gate. This is idempotent.
	if err := provision(ctx, bareDir, absRoot, upstreamURL, p.ReposDir(), existing != nil); err != nil {
		return nil, false, err
	}

	// Detect default branch from upstream remote.
	branch := git.DefaultBranch(ctx, absRoot, "origin")

	if existing != nil {
		var repo *db.Repo
		if forkURL != "" {
			repo, err = d.UpdateRepoMetadataWithFork(existing.ID, redactedUpstreamURL, forkURL, branch)
		} else {
			repo, err = d.UpdateRepoMetadata(existing.ID, redactedUpstreamURL, branch)
		}
		if err != nil {
			return nil, false, fmt.Errorf("update repo metadata: %w", err)
		}
		slog.Info("gate refreshed", "repo_id", repo.ID, "path", absRoot)
		return repo, false, nil
	}

	// Insert repo record with deterministic ID.
	repo, err := insertRepoRecord(d, id, absRoot, redactedUpstreamURL, forkURL, branch)
	if err != nil {
		// Rollback: undo what this attempt added, and nothing else.
		return nil, false, fmt.Errorf("insert repo: %w", err)
	}

	slog.Info("gate initialized", "repo_id", id, "path", absRoot, "upstream", redactedUpstreamURL)
	return repo, gateDirCreated, nil
}

// pathPresent reports whether anything at all - a directory, a file or a
// symlink, dangling or not - is at path.
func pathPresent(path string) (bool, error) {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return true, nil
	case os.IsNotExist(err):
		return false, nil
	default:
		return false, err
	}
}

// dirExists reports whether a path is a real directory, not a symlink to one.
func dirExists(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir()
}

// remotePointsAt reports whether the gate remote of a working repo resolves
// to bareDir.
func remotePointsAt(ctx context.Context, absRoot, bareDir string) bool {
	url, err := git.GetRemoteURL(ctx, absRoot, RemoteName)
	return err == nil && url == bareDir
}

// remoteState is the literal configuration of a working repo's gate remote:
// whether it exists and the url values written in its config, exactly as
// written (url.*.insteadOf is NOT applied, so an alias stays an alias).
type remoteState struct {
	present bool
	urls    []string
}

func (s remoteState) equal(o remoteState) bool {
	if s.present != o.present || len(s.urls) != len(o.urls) {
		return false
	}
	for i := range s.urls {
		if s.urls[i] != o.urls[i] {
			return false
		}
	}
	return true
}

// snapshotWorkingRemote reads the literal state of a working repo's gate
// remote.
func snapshotWorkingRemote(ctx context.Context, absRoot string) (remoteState, error) {
	has, err := git.HasRemote(ctx, absRoot, RemoteName)
	if err != nil {
		return remoteState{}, err
	}
	if !has {
		return remoteState{}, nil
	}
	urls, err := git.GetConfiguredRemoteURLs(ctx, absRoot, RemoteName)
	if err != nil {
		return remoteState{}, err
	}
	return remoteState{present: true, urls: urls}, nil
}

// rollbackWorkingRemote returns the gate remote of a working repo to the
// literal state this attempt found it in - but only when this attempt changed
// it. An unchanged remote is not written at all, so an alias, a multi-valued
// url or anything else the operator configured survives a failed init as it
// was.
//
// The remote is part of the caller's checkout, not of the gate, and a failed
// init has no business leaving a checkout wired to a gate that is not there.
func rollbackWorkingRemote(ctx context.Context, absRoot string, prior remoteState) error {
	now, err := snapshotWorkingRemote(ctx, absRoot)
	if err != nil {
		return fmt.Errorf("inspect checkout remote during rollback: %w", err)
	}
	if now.equal(prior) {
		return nil
	}
	if !prior.present {
		return git.RemoveRemote(ctx, absRoot, RemoteName)
	}
	key := "remote." + RemoteName + ".url"
	if len(now.urls) > 0 {
		if _, err := git.Run(ctx, absRoot, "config", "--unset-all", key); err != nil {
			return err
		}
	}
	for _, url := range prior.urls {
		if _, err := git.Run(ctx, absRoot, "config", "--add", key, url); err != nil {
			return err
		}
	}
	return nil
}

// rollbackGate removes the gate directory of a failed init - but only when
// this attempt created it and no registration owns it.
//
// Anything that was already at that path belongs to another registration, to
// an earlier interrupted attempt or to the operator, and removing it is the
// destruction issue #160 describes: the database row and the run records
// stayed while the objects, the refs and the linked-worktree administration
// of the gate were deleted.
//
// The init lock keeps other inits out while this attempt runs, but the
// registry is still consulted before the directory goes: a registration that
// owns the ID owns its gate, however it got there, and an unreadable registry
// leaves the directory alone as well - an owner that cannot be ruled out is
// an owner that must not be destroyed.
func rollbackGate(d *db.DB, bareDir, id string, created bool) error {
	if !created {
		return nil
	}
	owner, err := d.GetRepo(id)
	if err != nil {
		return fmt.Errorf("check gate owner during rollback: %w", err)
	}
	if owner != nil {
		return nil
	}
	return os.RemoveAll(bareDir)
}

func validateForkRouting(ctx context.Context, upstreamURL, forkURL string) error {
	parentProvider := scm.DetectProviderContext(ctx, upstreamURL)
	forkProvider := scm.DetectProviderContext(ctx, forkURL)
	if parentProvider == scm.ProviderGitHub && forkProvider == scm.ProviderGitHub {
		if github.RepoSlug(upstreamURL) == "" || github.RepoSlug(forkURL) == "" {
			return fmt.Errorf("fork URL routing requires GitHub parent and fork remotes with owner/repo paths")
		}
		return nil
	}
	return fmt.Errorf("fork URL routing is currently supported only for GitHub parent and fork remotes (parent provider: %s, fork provider: %s)", parentProvider, forkProvider)
}

// provisionGate creates or repairs the on-disk gate for a repo: the bare repo,
// its push/hook configuration, hook-path isolation, and the git remotes wiring
// the working repo to the gate and the gate to its upstream. Every step is
// idempotent so this doubles as the repair path for re-running init.
func provisionGate(ctx context.Context, bareDir, absRoot, upstreamURL, reposDir string, refresh bool) error {
	// Validate an existing gate without reinitializing its configuration.
	if err := git.InitBare(ctx, bareDir); err != nil {
		return fmt.Errorf("create bare repo: %w", err)
	}
	return configureGate(ctx, bareDir, absRoot, upstreamURL, reposDir, refresh)
}

// configureGate receives a validated bare gate from init or provisionGate.
func configureGate(ctx context.Context, bareDir, absRoot, upstreamURL, reposDir string, refresh bool) error {
	if _, err := git.RunBare(ctx, bareDir, "config", "receive.advertisePushOptions", "true"); err != nil {
		return fmt.Errorf("enable push options: %w", err)
	}

	if err := git.RefreshManagedGateHooks(bareDir); err != nil {
		return fmt.Errorf("install hooks: %w", err)
	}

	// Pin core.hookspath in the bare's per-worktree config so subprocess
	// writes to shared local config (e.g. husky during pnpm install) can't
	// disable the gate hook. See git.IsolateHooksPath for details.
	isolated, err := ensureGateHooksPathIsolation(ctx, bareDir)
	if err != nil {
		return fmt.Errorf("isolate hooks path: %w", err)
	}

	// Record upstream as origin on the gate repo so gh can resolve repository
	// context from detached worktrees created from the gate.
	if err := git.EnsureRemote(ctx, bareDir, "origin", upstreamURL); err != nil {
		return fmt.Errorf("add gate origin remote: %w", err)
	}

	if err := ensureWorkingRemote(ctx, absRoot, bareDir, reposDir, refresh); err != nil {
		return fmt.Errorf("add remote: %w", err)
	}

	if isolated {
		if err := git.MarkGateConfigCurrent(bareDir); err != nil {
			return fmt.Errorf("stamp gate config: %w", err)
		}
	}
	return nil
}

// ensureWorkingRemote points the working checkout's "no-mistakes" remote at the
// gate.
//
// The remote lives in the checkout's SHARED configuration, so it is inherited
// by every worktree of that checkout - which is exactly what a gate remote
// has to be. It is therefore written as rarely as possible: a refresh that
// finds the remote already pointing at the gate writes nothing at all, so
// re-registering a checkout never rewrites the configuration the operator's
// own tools share (issue #183).
func ensureWorkingRemote(ctx context.Context, absRoot, bareDir, reposDir string, refresh bool) error {
	existingURL, err := git.GetRemoteURL(ctx, absRoot, RemoteName)
	if err != nil {
		return git.AddRemote(ctx, absRoot, RemoteName, bareDir)
	}
	if existingURL == bareDir {
		// Already pointing at this gate. Nothing to write.
		return nil
	}
	if !refresh {
		// A leftover remote pointing into our own repos dir is stale gate wiring
		// (e.g. the working directory was copied, or its gate was half-ejected);
		// repoint it. Anything else is a user-managed remote we must not touch.
		if filepath.Dir(existingURL) == reposDir {
			return git.EnsureRemote(ctx, absRoot, RemoteName, bareDir)
		}
		return fmt.Errorf("remote %q already exists with url %q", RemoteName, existingURL)
	}
	return git.EnsureRemote(ctx, absRoot, RemoteName, bareDir)
}

// reattachRelocatedRepo detects a working directory that was renamed or moved
// after init: it carries a no-mistakes remote pointing at a gate in our repos
// dir, but its repo record references the old path. When the old path no
// longer exists, the record is migrated to the new path so the existing gate
// and its run history are reattached. It returns nil when the repo should be
// treated as a fresh init instead: no gate remote, an orphan gate with no
// record, or a copy whose original still exists on disk.
func reattachRelocatedRepo(ctx context.Context, d *db.DB, p *paths.Paths, absRoot string) (*db.Repo, error) {
	remoteURL, err := git.GetRemoteURL(ctx, absRoot, RemoteName)
	if err != nil {
		return nil, nil
	}
	id := strings.TrimSuffix(filepath.Base(remoteURL), ".git")
	if p.RepoDir(id) != remoteURL {
		// Not one of our gate paths; fresh init decides what to do with it.
		return nil, nil
	}
	repo, err := d.GetRepo(id)
	if err != nil {
		return nil, fmt.Errorf("look up relocated repo: %w", err)
	}
	if repo == nil {
		return nil, nil
	}
	if _, err := os.Stat(repo.WorkingPath); err == nil {
		// The recorded checkout still exists, so absRoot is a copy of it, not
		// a move; the copy gets its own gate.
		return nil, nil
	}
	migrated, err := d.UpdateRepoWorkingPath(id, absRoot)
	if err != nil {
		return nil, fmt.Errorf("migrate repo working path: %w", err)
	}
	slog.Info("gate reattached after working dir move", "repo_id", id, "old_path", repo.WorkingPath, "new_path", absRoot)
	return migrated, nil
}

// Eject removes the no-mistakes gate from the repo at workDir.
// It removes the remote, deletes the bare repo and worktrees,
// and deletes the repo record from the database.
func Eject(ctx context.Context, d *db.DB, p *paths.Paths, workDir string) (*db.Repo, error) {
	if classified, err := (gatecontext.Inspector{DB: d, Paths: p}).Inspect(ctx, gatecontext.Request{CWD: workDir, MarkerPresent: gatecontext.MarkerPresent()}); err != nil {
		return nil, err
	} else if classified.Nested {
		return nil, fmt.Errorf("%s", gatecontext.RefusalMessage(classified))
	}
	// Normalize worktrees back to the main repo root so eject works no matter
	// which checkout the user runs it from.
	gitRoot, err := git.FindMainRepoRoot(workDir)
	if err != nil {
		return nil, fmt.Errorf("find git root: %w", err)
	}
	absRoot := gitRoot

	// Eject removes a gate directory, so it takes the init lock: an init
	// deciding who owns a repo ID must not see the gate vanish under it.
	unlock, err := AcquireInitLock(ctx, p)
	if err != nil {
		return nil, err
	}
	defer unlock()

	// Look up repo in DB.
	repo, err := d.GetRepoByPath(absRoot)
	if err != nil {
		return nil, fmt.Errorf("get repo: %w", err)
	}
	if repo == nil {
		return nil, fmt.Errorf("not initialized for %s", absRoot)
	}

	// Remove remote from working repo (non-fatal).
	_ = git.RemoveRemote(ctx, absRoot, RemoteName)

	// Delete bare repo.
	bareDir := p.RepoDir(repo.ID)
	os.RemoveAll(bareDir)

	// Delete worktrees for this repo. This happens before the repo record is
	// deleted, because in a configured root the run rows are what identify
	// which directories are ours to remove.
	removeRepoWorktrees(d, p, repo)

	// Delete repo record (cascades to runs + steps).
	if err := d.DeleteRepo(repo.ID); err != nil {
		return nil, fmt.Errorf("delete repo record: %w", err)
	}

	slog.Info("gate ejected", "repo_id", repo.ID, "path", absRoot)
	return repo, nil
}

// removeRepoWorktrees deletes the ejected repository's run worktrees.
//
// Under the default placement no-mistakes owns <NM_HOME>/worktrees/<repoID>
// outright, so the whole directory goes. Everything a run recorded outside it
// is removed one directory at a time (see worktree_roots and
// internal/worktrees): such a directory sits in a directory of the operator's
// own - it holds the toolchain configuration the runs were placed there to
// inherit - so eject removes exactly what this repository's own run rows
// recorded, and touches nothing else: not the root, not the operator's files,
// and not a neighbouring directory that merely looks like a run. Reading the
// recorded placement rather than deriving it is also what reaches a run left in
// a root the operator has since reconfigured away.
//
// Every one of those directories is swept before any of them is removed - both
// halves, in the one process snapshot that costs (see
// procreap.SweepRunWorktrees). Which half a run landed in says nothing about
// whether it leaked a process that escaped its group, and a default-placement
// directory removed without a sweep leaves that process burning CPU on a deleted
// cwd until some later daemon startup happens to sweep the tree by shape - which
// is the cost this sweep exists to eliminate, not to defer.
//
// The sweep also has to precede the deletion of the repository record: the
// cascade takes the run rows with it, and outside the default tree those rows are
// the only thing that can name the directory. Sweeping afterwards would be
// sweeping a directory nothing knows about.
//
// Failures are logged rather than fatal: an eject that cannot delete a leftover
// worktree must still finish removing the gate.
func removeRepoWorktrees(d *db.DB, p *paths.Paths, repo *db.Repo) {
	defaultDir := filepath.Join(p.WorktreesDir(), repo.ID)

	runs, err := d.GetRunsByRepo(repo.ID)
	if err != nil {
		slog.Warn("failed to list runs while removing worktrees during eject", "repo_id", repo.ID, "error", err)
		os.RemoveAll(defaultDir)
		return
	}
	var recorded []string
	var sweepable []procreap.Worktree
	for _, run := range runs {
		path := worktrees.RecordedDir(p, run.WorktreePath(), repo.ID, run.ID)
		if !worktrees.Contains(defaultDir, path) {
			// Everything in the default tree goes with the directory we own
			// outright, so only the rest is removed one path at a time.
			recorded = append(recorded, path)
		}
		if reachableRunWorktree(path, run.Status) {
			sweepable = append(sweepable, procreap.Worktree{Dir: path, RepoID: repo.ID, RunID: run.ID})
		}
	}
	sweepRunWorktrees(p.WorktreesDir(), sweepable, "eject")

	os.RemoveAll(defaultDir)
	for _, path := range recorded {
		if err := os.RemoveAll(path); err != nil {
			slog.Warn("failed to remove run worktree during eject", "path", path, "error", err)
		}
	}
}

// reachableRunWorktree reports whether a recorded placement can still have
// anything standing in it, which is the same bound daemon startup applies to the
// same set (see leftoverRecordedRunWorktrees and db.ActiveRunWorktreesOutside):
// a directory that is still on disk, or the worktree of a run that never
// reached a terminal state and may therefore have lost its directory while a
// process it leaked kept holding it.
//
// A terminal run whose directory is already gone is not in reach: its removal
// swept it, and eject would otherwise pay a full sweep for every run this
// repository has ever had - run rows are never pruned, so that set grows without
// bound while the work it does is empty.
func reachableRunWorktree(dir string, status types.RunStatus) bool {
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return true
	}
	return status == types.RunPending || status == types.RunRunning
}
