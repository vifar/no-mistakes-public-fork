package gate

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// Use real SQLite refusal rather than relying only on the preserved test seam.
func TestInitComponentSQLiteFailurePreservesGate(t *testing.T) {
	for _, kind := range []string{"adopt", "refresh", "fresh"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			p, d := newInitFixture(t)
			work := setupTestRepo(t)
			gateDir := p.RepoDir(repoID(work))
			var before map[string]string
			if kind == "refresh" {
				if _, _, err := Init(ctx, d, p, work); err != nil {
					t.Fatal(err)
				}
			} else if kind == "adopt" {
				initBareAt(t, gateDir)
				if err := gitpkg.AddRemote(ctx, work, RemoteName, gateDir); err != nil {
					t.Fatal(err)
				}
			}
			var commit, linked string
			if kind != "fresh" {
				commit, linked = seedGateSentinel(t, gateDir)
				if err := os.WriteFile(filepath.Join(gateDir, "hooks", "pre-receive"), []byte("#!/bin/sh\nexit 7\n"), 0o751); err != nil {
					t.Fatal(err)
				}
				before = treeSnapshot(t, gateDir)
			}
			raw, err := sql.Open("sqlite", p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := raw.Close(); err != nil {
					t.Error(err)
				}
			}()
			event := "INSERT"
			if kind == "refresh" {
				event = "UPDATE"
			}
			_, err = raw.Exec("CREATE TRIGGER refuse_init BEFORE " + event + " ON repos BEGIN SELECT RAISE(ABORT, 'component SQLite refusal'); END")
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = Init(ctx, d, p, work)
			if err == nil || !strings.Contains(err.Error(), "component SQLite refusal") {
				t.Fatalf("unexpected failure: %v", err)
			}
			if kind == "fresh" {
				if _, err := os.Lstat(gateDir); !os.IsNotExist(err) {
					t.Fatalf("init-created directory survived: %v", err)
				}
				if has, err := gitpkg.HasRemote(ctx, work, RemoteName); err != nil || has {
					t.Fatalf("new remote survived: %v %v", has, err)
				}
			} else {
				compareSnapshots(t, "real SQLite refusal", before, treeSnapshot(t, gateDir))
				assertGateWorktreeUsable(t, linked, commit)
			}
			repo, err := d.GetRepoByPath(work)
			if err != nil {
				t.Fatal(err)
			}
			if (repo != nil) != (kind == "refresh") {
				t.Fatalf("wrong registration after refusal: %+v", repo)
			}
		})
	}
}

func TestInitComponentProvisionFailurePreservesGate(t *testing.T) {
	for _, registered := range []bool{false, true} {
		t.Run(fmt.Sprint(registered), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p, d := newInitFixture(t)
			work := setupTestRepo(t)
			gateDir := p.RepoDir(repoID(work))
			if registered {
				if _, _, err := Init(ctx, d, p, work); err != nil {
					t.Fatal(err)
				}
			} else {
				initBareAt(t, gateDir)
				if err := gitpkg.AddRemote(ctx, work, RemoteName, gateDir); err != nil {
					t.Fatal(err)
				}
			}
			commit, linked := seedGateSentinel(t, gateDir)
			if err := os.WriteFile(filepath.Join(gateDir, "hooks", "pre-receive"), []byte("#!/bin/sh\nexit 9\n"), 0o751); err != nil {
				t.Fatal(err)
			}
			before := treeSnapshot(t, gateDir)
			original := ensureGateHooksPathIsolation
			ensureGateHooksPathIsolation = func(context.Context, string) (bool, error) { cancel(); return false, context.Canceled }
			t.Cleanup(func() { ensureGateHooksPathIsolation = original })
			if _, _, err := Init(ctx, d, p, work); !errors.Is(err, context.Canceled) {
				t.Fatalf("want cancellation, got %v", err)
			}
			compareSnapshots(t, "partially provisioned gate", before, treeSnapshot(t, gateDir))
			assertGateWorktreeUsable(t, linked, commit)
		})
	}
}

func TestInitComponentUnsafeMetadataFailsBeforeMutation(t *testing.T) {
	ctx := context.Background()
	p, d := newInitFixture(t)
	work := setupTestRepo(t)
	gateDir := p.RepoDir(repoID(work))
	initBareAt(t, gateDir)
	if err := gitpkg.AddRemote(ctx, work, RemoteName, gateDir); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "external-hook")
	data := []byte("#!/bin/sh\nexit 3\n")
	if err := os.WriteFile(external, data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(gateDir, "hooks", "pre-receive")); err != nil {
		t.Fatal(err)
	}
	before := treeSnapshot(t, gateDir)
	if _, _, err := Init(ctx, d, p, work); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("unsafe metadata not refused: %v", err)
	}
	compareSnapshots(t, "symlink refusal", before, treeSnapshot(t, gateDir))
	after, err := os.ReadFile(external)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, after) {
		t.Fatal("external target changed")
	}
}

func TestInitComponentMissingHooksFailureRestoresAbsentDirectory(t *testing.T) {
	ctx := context.Background()
	p, d := newInitFixture(t)
	work := setupTestRepo(t)
	gateDir := p.RepoDir(repoID(work))
	initBareAt(t, gateDir)
	hooks := filepath.Join(gateDir, "hooks")
	entries, err := os.ReadDir(hooks)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if err := os.Remove(filepath.Join(hooks, entry.Name())); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(hooks); err != nil {
		t.Fatal(err)
	}
	if err := gitpkg.AddRemote(ctx, work, RemoteName, gateDir); err != nil {
		t.Fatal(err)
	}
	before := treeSnapshot(t, gateDir)
	original := insertRepoRecord
	insertRepoRecord = func(*db.DB, string, string, string, string, string) (*db.Repo, error) {
		return nil, errors.New("forced insert failure")
	}
	t.Cleanup(func() { insertRepoRecord = original })
	if _, _, err := Init(ctx, d, p, work); err == nil {
		t.Fatal("missing error")
	}
	compareSnapshots(t, "absent hooks directory", before, treeSnapshot(t, gateDir))
}

// The process entry is test-only and refuses roots outside OS disposable storage.
func TestInitComponentProcessEntry(t *testing.T) {
	encoded := os.Getenv("NM160_COMPONENT_PROCESS")
	if encoded == "" {
		return
	}
	var input struct{ Root, Work string }
	if err := json.Unmarshal([]byte(encoded), &input); err != nil {
		t.Fatal(err)
	}
	temp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{input.Root, input.Work} {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatal(err)
		}
		relative, err := filepath.Rel(temp, resolved)
		if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			t.Fatal("refuse non-disposable process fixture")
		}
	}
	p := paths.WithRoot(input.Root)
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := d.Close(); err != nil {
			t.Error(err)
		}
	}()
	fmt.Println("READY")
	repo, created, initErr := Init(context.Background(), d, p, input.Work)
	result := struct {
		ID      string
		Created bool
		Failure string
	}{Created: created}
	if repo != nil {
		result.ID = repo.ID
	}
	if initErr != nil {
		result.Failure = initErr.Error()
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		t.Fatal(err)
	}
}

func TestInitComponentIndependentProcesses(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		t.Run(fmt.Sprint(refuse), func(t *testing.T) {
			p, d := newInitFixture(t)
			work := setupTestRepo(t)
			gateDir := p.RepoDir(repoID(work))
			var before map[string]string
			if refuse {
				initBareAt(t, gateDir)
				if err := gitpkg.AddRemote(context.Background(), work, RemoteName, gateDir); err != nil {
					t.Fatal(err)
				}
				before = treeSnapshot(t, gateDir)
				raw, err := sql.Open("sqlite", p.DB())
				if err != nil {
					t.Fatal(err)
				}
				_, writeErr := raw.Exec("CREATE TRIGGER refuse_init BEFORE INSERT ON repos BEGIN SELECT RAISE(ABORT, 'component process refusal'); END")
				if err := errors.Join(writeErr, raw.Close()); err != nil {
					t.Fatal(err)
				}
			}
			lock, err := os.OpenFile(initLockPath(p), os.O_CREATE|os.O_RDWR, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if err := tryLockInitFile(lock); err != nil {
				t.Fatal(err)
			}
			// The parent lock keeps all four live processes behind one admission point.
			released := false
			defer func() {
				if !released {
					if err := unlockInitFile(lock); err != nil {
						t.Error(err)
					}
				}
				if err := lock.Close(); err != nil {
					t.Error(err)
				}
			}()
			encoded, err := json.Marshal(struct{ Root, Work string }{p.Root(), work})
			if err != nil {
				t.Fatal(err)
			}
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			type child struct {
				cmd    *exec.Cmd
				reader *bufio.Reader
				stderr *bytes.Buffer
				waited bool
			}
			children := make([]*child, 0, 4)
			t.Cleanup(func() {
				cancel()
				for _, c := range children {
					if !c.waited {
						if err := c.cmd.Wait(); err != nil {
							t.Errorf("reap unfinished fixture process: %v", err)
						}
						c.waited = true
					}
				}
			})
			for i := 0; i < 4; i++ {
				cmd := exec.CommandContext(ctx, exe, "-test.run=^TestInitComponentProcessEntry$")
				cmd.Env = append(os.Environ(), "NM160_COMPONENT_PROCESS="+string(encoded))
				stderr := new(bytes.Buffer)
				cmd.Stderr = stderr
				stdout, err := cmd.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				children = append(children, &child{cmd: cmd, reader: bufio.NewReader(stdout), stderr: stderr})
			}
			for _, c := range children {
				line, err := c.reader.ReadString('\n')
				if err != nil || line != "READY\n" {
					t.Fatalf("process readiness: %q %v", line, err)
				}
			}
			for _, c := range children {
				t.Logf("independent process ready behind admission lock: pid=%d", c.cmd.Process.Pid)
			}
			if err := unlockInitFile(lock); err != nil {
				t.Fatal(err)
			}
			released = true
			createdCount := 0
			for _, c := range children {
				line, err := c.reader.ReadBytes('\n')
				if err != nil {
					t.Fatal(err)
				}
				var result struct {
					ID      string
					Created bool
					Failure string
				}
				if err := json.Unmarshal(line, &result); err != nil {
					t.Fatal(err)
				}
				if _, err := io.Copy(io.Discard, c.reader); err != nil {
					t.Error(err)
				}
				waitErr := c.cmd.Wait()
				c.waited = true
				if waitErr != nil {
					t.Fatalf("child exit: %v %s", waitErr, c.stderr)
				}
				if result.Created {
					createdCount++
				}
				if refuse {
					if !strings.Contains(result.Failure, "component process refusal") {
						t.Fatalf("wrong failure: %+v", result)
					}
				} else if result.Failure != "" || result.ID != repoID(work) {
					t.Fatalf("wrong success: %+v", result)
				}
			}
			t.Logf("all four OS processes exited; refused=%v, created=%d", refuse, createdCount)
			if refuse {
				if createdCount != 0 {
					t.Fatal("failed adoption claimed gate creation")
				}
				compareSnapshots(t, "cross-process failed adoptions", before, treeSnapshot(t, gateDir))
				if repo, err := d.GetRepoByPath(work); err != nil || repo != nil {
					t.Fatalf("registration leaked: %+v %v", repo, err)
				}
			} else {
				if createdCount != 1 {
					t.Fatalf("created count %d, want 1", createdCount)
				}
				if repo, err := d.GetRepoByPath(work); err != nil || repo == nil {
					t.Fatalf("missing registration: %+v %v", repo, err)
				}
			}
		})
	}
}

// A conflicting filesystem type at rollback is never deleted to fake success.
func TestInitComponentRollbackFailureIsReported(t *testing.T) {
	ctx := context.Background()
	p, d := newInitFixture(t)
	work := setupTestRepo(t)
	gateDir := p.RepoDir(repoID(work))
	initBareAt(t, gateDir)
	if err := gitpkg.AddRemote(ctx, work, RemoteName, gateDir); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("registration refused before rollback conflict")
	original := insertRepoRecord
	changed := filepath.Join(gateDir, "config.worktree")
	insertRepoRecord = func(*db.DB, string, string, string, string, string) (*db.Repo, error) {
		if err := os.Remove(changed); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(changed, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(changed, "other-writer"), []byte("retain this"), 0o600); err != nil {
			t.Fatal(err)
		}
		return nil, failure
	}
	t.Cleanup(func() { insertRepoRecord = original })
	_, _, err := Init(ctx, d, p, work)
	if !errors.Is(err, failure) || !strings.Contains(err.Error(), "restore gate metadata") || !strings.Contains(err.Error(), "refusing replacement") {
		t.Fatalf("cleanup error lost: %v", err)
	}
	value, readErr := os.ReadFile(filepath.Join(changed, "other-writer"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(value) != "retain this" {
		t.Fatal("conflicting writer data lost")
	}
}
