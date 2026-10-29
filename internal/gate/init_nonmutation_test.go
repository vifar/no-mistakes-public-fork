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
	"reflect"
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

func TestInitComponentSymlinkMetadata(t *testing.T) {
	for _, layout := range []string{"custom-post", "custom-hooks", "hooks-directory", "managed-hooks", "config", "config-chain", "dangling-config", "markers", "opaque-companion", "dangling-companion"} {
		for _, kind := range []string{"adopt", "refresh"} {
			for _, refuse := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/refuse=%v", layout, kind, refuse), func(t *testing.T) {
					ctx := context.Background()
					p, d := newInitFixture(t)
					work := setupTestRepo(t)
					gateDir := p.RepoDir(repoID(work))
					if kind == "refresh" {
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
					previousRepo, err := d.GetRepoByPath(work)
					if err != nil {
						t.Fatal(err)
					}
					var previousRun *db.Run
					if previousRepo != nil {
						run, err := d.InsertRun(previousRepo.ID, "sentinel", commit, commit)
						if err != nil {
							t.Fatal(err)
						}
						previousRun, err = d.GetRun(run.ID)
						if err != nil {
							t.Fatal(err)
						}
					}
					targets := resolveSymlinks(t, t.TempDir())
					protected := make(map[string][]byte)
					links := make(map[string]string)
					link := func(name string, data []byte, dangling, protect bool) {
						t.Helper()
						path := filepath.Join(gateDir, name)
						target := filepath.Join(targets, fmt.Sprintf("target-%d", len(links)))
						if !dangling {
							if err := os.WriteFile(target, data, 0o751); err != nil {
								t.Fatal(err)
							}
							if protect {
								protected[target] = data
							}
						}
						if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
							t.Fatal(err)
						}
						parent, err := filepath.EvalSymlinks(filepath.Dir(path))
						if err != nil {
							t.Fatal(err)
						}
						text, err := filepath.Rel(parent, target)
						if err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink(text, path); err != nil {
							t.Fatal(err)
						}
						if !dangling {
							got, err := os.ReadFile(path)
							if err != nil || !bytes.Equal(data, got) {
								t.Fatalf("invalid symlink fixture: %s %v", path, err)
							}
						}
						links[name] = text
					}
					var retainedPath string
					switch layout {
					case "custom-post":
						link("hooks/post-receive", []byte("#!/bin/sh\nexit 4\n"), false, true)
						retainedPath = filepath.Join(gateDir, "hooks/post-receive")
					case "hooks-directory":
						hooks := filepath.Join(gateDir, "hooks")
						target := filepath.Join(targets, "hook directory")
						if err := os.Rename(hooks, target); err != nil {
							t.Fatal(err)
						}
						text, err := filepath.Rel(resolveSymlinks(t, gateDir), target)
						if err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink(text, hooks); err != nil {
							t.Fatal(err)
						}
						links["hooks"] = text
						retainedPath = hooks
						fallthrough
					case "custom-hooks":
						link("hooks/pre-receive", []byte("#!/bin/sh\nexit 3\n"), false, true)
						link("hooks/post-receive", []byte("#!/bin/sh\nexit 4\n"), false, true)
						if retainedPath == "" {
							retainedPath = filepath.Join(gateDir, "hooks/post-receive")
						}
					case "managed-hooks":
						link("hooks/pre-receive", []byte("#!/bin/sh\n# no-mistakes pre-receive hook\n# daemon admit-push\nexit 0\n"), false, true)
						link("hooks/post-receive", []byte("#!/bin/sh\n# no-mistakes post-receive hook\n# daemon notify-push\nexit 0\n"), false, true)
					case "config", "config-chain":
						for _, name := range []string{"config", "config.worktree"} {
							data, err := os.ReadFile(filepath.Join(gateDir, name))
							if os.IsNotExist(err) {
								data = []byte("[test]\n  value = original\n")
							} else if err != nil {
								t.Fatal(err)
							}
							link(name, data, false, false)
							if layout == "config-chain" {
								path := filepath.Join(gateDir, name)
								leaf := filepath.Clean(filepath.Join(filepath.Dir(path), links[name]))
								hop := leaf + "-hop"
								if err := os.Symlink(leaf, hop); err != nil {
									t.Fatal(err)
								}
								if err := os.Remove(path); err != nil {
									t.Fatal(err)
								}
								if err := os.Symlink(hop, path); err != nil {
									t.Fatal(err)
								}
								links[name] = hop
							}
						}
					case "dangling-config":
						link("config.worktree", nil, true, false)
						if _, err := gitpkg.RunBare(ctx, gateDir, "config", "--local", "core.bare", "true"); err != nil {
							t.Fatal(err)
						}
					case "markers":
						link("no-mistakes-gate-config", []byte("old stamp\n"), false, true)
						link("no-mistakes-shared-config.json", []byte("{}\n"), false, true)
					case "opaque-companion", "dangling-companion":
						script, err := gitpkg.PreReceiveHookScript(gateDir)
						if err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(gateDir, "hooks/pre-receive"), []byte(script), 0o755); err != nil {
							t.Fatal(err)
						}
						retainedPath = filepath.Join(gateDir, "hooks/pre-receive.no-mistakes-user")
						if layout == "opaque-companion" {
							if err := os.Mkdir(retainedPath, 0o751); err != nil {
								t.Fatal(err)
							}
							if err := os.WriteFile(filepath.Join(retainedPath, "unowned"), []byte("retain"), 0o600); err != nil {
								t.Fatal(err)
							}
						} else {
							link("hooks/pre-receive.no-mistakes-user", nil, true, true)
						}
					}
					var retained os.FileInfo
					if retainedPath != "" {
						var err error
						retained, err = os.Lstat(retainedPath)
						if err != nil {
							t.Fatal(err)
						}
					}
					var originalPreReceive os.FileInfo
					if layout == "custom-hooks" || layout == "hooks-directory" {
						var err error
						originalPreReceive, err = os.Lstat(filepath.Join(gateDir, "hooks/pre-receive"))
						if err != nil {
							t.Fatal(err)
						}
					}
					before, externalBefore := treeSnapshot(t, gateDir), treeSnapshot(t, targets)
					raw, err := sql.Open("sqlite", p.DB())
					if err != nil {
						t.Fatal(err)
					}
					defer func() {
						if err := raw.Close(); err != nil {
							t.Error(err)
						}
					}()
					if refuse {
						event := "INSERT"
						if kind == "refresh" {
							event = "UPDATE"
						}
						if _, err := raw.Exec("CREATE TRIGGER refuse_link_init BEFORE " + event + " ON repos BEGIN SELECT RAISE(ABORT, 'symlink SQLite refusal'); END"); err != nil {
							t.Fatal(err)
						}
					}
					_, created, err := Init(ctx, d, p, work)
					if refuse {
						if err == nil || !strings.Contains(err.Error(), "symlink SQLite refusal") {
							t.Fatalf("supported layout failed before SQLite: %v", err)
						}
						compareSnapshots(t, "symlink gate rollback", before, treeSnapshot(t, gateDir))
						compareSnapshots(t, "symlink target rollback", externalBefore, treeSnapshot(t, targets))
					} else if err != nil || created {
						t.Fatalf("supported layout was not enrolled: created=%v err=%v", created, err)
					}
					if originalPreReceive != nil {
						name := "hooks/pre-receive"
						if !refuse {
							name += ".no-mistakes-user"
						}
						info, err := os.Lstat(filepath.Join(gateDir, name))
						if err != nil || !os.SameFile(originalPreReceive, info) {
							t.Fatalf("custom pre-receive entry lost its identity: %v", err)
						}
					}
					for path, data := range protected {
						after, err := os.ReadFile(path)
						if err != nil || !bytes.Equal(data, after) {
							t.Fatalf("custom target changed at %s: %q %v", path, after, err)
						}
					}
					if retained != nil {
						after, err := os.Lstat(retainedPath)
						if err != nil || !os.SameFile(retained, after) {
							t.Fatalf("untouched entry identity changed: %s %v", retainedPath, err)
						}
					}
					if !refuse {
						for name, text := range links {
							switch layout {
							case "managed-hooks", "markers":
								info, err := os.Lstat(filepath.Join(gateDir, name))
								if err != nil || !info.Mode().IsRegular() {
									t.Fatalf("managed entry was not refreshed: %s %v", name, err)
								}
								continue
							case "custom-hooks", "hooks-directory":
								if name == "hooks/pre-receive" {
									name += ".no-mistakes-user"
								}
							}
							got, err := os.Readlink(filepath.Join(gateDir, name))
							if err != nil || got != text {
								t.Fatalf("link text changed at %s: %q %v", name, got, err)
							}
						}
					}
					if previousRepo != nil {
						got, err := d.GetRepo(previousRepo.ID)
						if err != nil || !reflect.DeepEqual(got, previousRepo) {
							t.Fatalf("registration changed: %+v %v", got, err)
						}
						gotRun, err := d.GetRun(previousRun.ID)
						if err != nil || !reflect.DeepEqual(gotRun, previousRun) {
							t.Fatalf("seeded run changed: %+v %v", gotRun, err)
						}
					} else {
						got, err := d.GetRepoByPath(work)
						if err != nil || (refuse && got != nil) || (!refuse && (got == nil || got.ID != repoID(work))) {
							t.Fatalf("wrong adoption registration: %+v %v", got, err)
						}
					}
					assertGateWorktreeUsable(t, linked, commit)
				})
			}
		}
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

func TestInitComponentFetchOnlyRemote(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		t.Run(fmt.Sprint(refuse), func(t *testing.T) {
			ctx := context.Background()
			p, d := newInitFixture(t)
			work := setupTestRepo(t)
			repo, _, err := Init(ctx, d, p, work)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := gitpkg.Run(ctx, work, "config", "--unset-all", "remote."+RemoteName+".url"); err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(work, ".git", "config")
			before, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			gateBefore := treeSnapshot(t, p.RepoDir(repo.ID))
			if refuse {
				raw, err := sql.Open("sqlite", p.DB())
				if err != nil {
					t.Fatal(err)
				}
				_, writeErr := raw.Exec("CREATE TRIGGER refuse_url_refresh BEFORE UPDATE ON repos BEGIN SELECT RAISE(ABORT, 'URL refresh refusal'); END")
				if err := errors.Join(writeErr, raw.Close()); err != nil {
					t.Fatal(err)
				}
			}
			_, created, err := Init(ctx, d, p, work)
			if created {
				t.Fatal("refresh claimed creation")
			}
			if refuse {
				if err == nil || !strings.Contains(err.Error(), "URL refresh refusal") {
					t.Fatalf("wrong refusal: %v", err)
				}
				after, err := os.ReadFile(configPath)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(before, after) {
					t.Fatalf("fetch-only remote config changed:\n%s", after)
				}
				compareSnapshots(t, "fetch-only refresh rollback", gateBefore, treeSnapshot(t, p.RepoDir(repo.ID)))
			} else {
				if err != nil {
					t.Fatal(err)
				}
				url, err := gitpkg.GetConfiguredRemoteURL(ctx, work, RemoteName)
				if err != nil || url != p.RepoDir(repo.ID) {
					t.Fatalf("fetch-only remote was not repaired: %q %v", url, err)
				}
			}
		})
	}
}

func TestInitComponentRemoteRollbackURLStates(t *testing.T) {
	for _, priorURLs := range [][]string{nil, {""}, {"literal:first", "literal:second"}} {
		for _, currentURLs := range [][]string{nil, {"literal:changed"}} {
			t.Run(fmt.Sprintf("%q-to-%q", currentURLs, priorURLs), func(t *testing.T) {
				ctx := context.Background()
				work := setupTestRepo(t)
				key := "remote." + RemoteName
				if _, err := gitpkg.Run(ctx, work, "config", key+".fetch", "+refs/heads/*:refs/remotes/no-mistakes/*"); err != nil {
					t.Fatal(err)
				}
				for _, url := range currentURLs {
					if _, err := gitpkg.Run(ctx, work, "config", "--add", key+".url", url); err != nil {
						t.Fatal(err)
					}
				}
				prior := remoteState{present: true, urls: priorURLs}
				if err := rollbackWorkingRemote(ctx, work, prior); err != nil {
					t.Fatal(err)
				}
				has, err := gitpkg.HasRemote(ctx, work, RemoteName)
				if err != nil || !has {
					t.Fatalf("lost remote: %v %v", has, err)
				}
				raw, err := gitpkg.RunRaw(ctx, work, "config", "--null", "--get-all", key+".url")
				if len(priorURLs) == 0 {
					if code, ok := gitpkg.ExitCode(err); !ok || code != 1 || len(raw) != 0 {
						t.Fatalf("URL key should be absent: %q %v", raw, err)
					}
				} else if err != nil || string(raw) != strings.Join(priorURLs, "\x00")+"\x00" {
					t.Fatalf("wrong literal URLs: %q %v", raw, err)
				}
				fetch, err := gitpkg.Run(ctx, work, "config", "--get", key+".fetch")
				if err != nil || fetch != "+refs/heads/*:refs/remotes/no-mistakes/*" {
					t.Fatalf("fetch configuration changed: %q %v", fetch, err)
				}
			})
		}
	}
}

func TestInitComponentRemoteConfigReadFailures(t *testing.T) {
	for _, broken := range []string{"corrupt", "unreadable", "cancelled"} {
		t.Run(broken, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			work := setupTestRepo(t)
			config := filepath.Join(work, ".git", "config")
			if broken == "corrupt" {
				if err := os.WriteFile(config, []byte("[broken\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			} else if broken == "unreadable" {
				if err := os.Remove(config); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(config, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if broken == "cancelled" {
				cancel()
			}
			if urls, err := gitpkg.GetConfiguredRemoteURLs(ctx, work, RemoteName); err == nil {
				t.Fatalf("actual config read error became URLs: %q", urls)
			}
			if _, err := snapshotWorkingRemote(ctx, work); err == nil {
				t.Fatal("snapshot accepted broken config")
			}
			if err := rollbackWorkingRemote(ctx, work, remoteState{present: true}); err == nil {
				t.Fatal("rollback accepted broken config")
			}
		})
	}
}

func TestInitComponentOccupiedDanglingCompanion(t *testing.T) {
	ctx := context.Background()
	p, d := newInitFixture(t)
	work := setupTestRepo(t)
	bare := p.RepoDir(repoID(work))
	initBareAt(t, bare)
	if err := gitpkg.AddRemote(ctx, work, RemoteName, bare); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bare, "hooks/pre-receive"), []byte("#!/bin/sh\nexit 4\n"), 0o751); err != nil {
		t.Fatal(err)
	}
	companion := filepath.Join(bare, "hooks/pre-receive.no-mistakes-user")
	if err := os.Symlink("missing-user-hook", companion); err != nil {
		t.Fatal(err)
	}
	before := treeSnapshot(t, bare)
	if _, _, err := Init(ctx, d, p, work); err == nil || !strings.Contains(err.Error(), "companion already exists") {
		t.Fatalf("occupied companion was overwritten: %v", err)
	}
	compareSnapshots(t, "occupied dangling companion", before, treeSnapshot(t, bare))
}
