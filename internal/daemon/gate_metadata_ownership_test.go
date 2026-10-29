package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
)

type metadataProcessInput struct {
	Root, Work, Mode string
	Wait             time.Duration
}

type metadataProcessResult struct {
	Failure string
	Stats   gateMigrationStats
	Created bool
}

func TestGateMetadataProcessEntry(t *testing.T) {
	encoded := os.Getenv("NM160_METADATA_PROCESS")
	if encoded == "" {
		return
	}
	var input metadataProcessInput
	if err := json.Unmarshal([]byte(encoded), &input); err != nil {
		t.Fatal(err)
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
	ctx, cancel := context.WithTimeout(context.Background(), input.Wait)
	defer cancel()
	var result metadataProcessResult
	switch input.Mode {
	case "init":
		_, result.Created, err = gate.Init(ctx, d, p, input.Work)
		if err != nil {
			result.Failure = err.Error()
		}
	case "migrate":
		result.Stats = migrateGateConfigs(ctx, d, p)
	default:
		t.Fatal("unknown fixture operation")
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("RESULT %s\n", data)
}

func metadataGateTree(t *testing.T, root string) map[string]string {
	t.Helper()
	result := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		name, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		var data []byte
		if info.Mode().IsRegular() {
			data, err = os.ReadFile(path)
		} else if info.Mode()&os.ModeSymlink != 0 {
			var link string
			link, err = os.Readlink(path)
			data = []byte(link)
		}
		result[name] = fmt.Sprintf("%s:%s", info.Mode(), data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestGateMetadataEnrollmentMigrationOwnership(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	helperDir := t.TempDir()
	helper := filepath.Join(helperDir, "git")
	if runtime.GOOS == "windows" {
		helper += ".exe"
	}
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", helper, "./testdata/gate-metadata-git").CombinedOutput(); err != nil {
		t.Fatalf("build Git boundary helper: %v\n%s", err, out)
	}
	realGit, err := testgit.RealGit()
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, first := range []string{"init", "migrate"} {
		t.Run(first+"-first", func(t *testing.T) {
			root := t.TempDir()
			p := paths.WithRoot(filepath.Join(root, "owner"))
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			d, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := d.Close(); err != nil {
					t.Error(err)
				}
			})
			work := filepath.Join(root, "source")
			upstream := filepath.Join(root, "upstream.git")
			if err := git.InitBare(ctx, upstream); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.CommandContext(ctx, realGit, "init", work).CombinedOutput(); err != nil {
				t.Fatalf("init source: %v %s", err, out)
			}
			if err := git.AddRemote(ctx, work, "origin", upstream); err != nil {
				t.Fatal(err)
			}
			repo, _, err := gate.Init(ctx, d, p, work)
			if err != nil {
				t.Fatal(err)
			}
			bare := p.RepoDir(repo.ID)
			tree, err := git.RunBare(ctx, bare, "mktree")
			if err != nil {
				t.Fatal(err)
			}
			sha, err := git.RunBare(ctx, bare, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit-tree", tree, "-m", "gate-only sentinel")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := git.RunBare(ctx, bare, "update-ref", "refs/gate-only/sentinel", sha); err != nil {
				t.Fatal(err)
			}
			linked := filepath.Join(root, "linked")
			if err := git.WorktreeAdd(ctx, bare, linked, sha); err != nil {
				t.Fatal(err)
			}
			run, err := d.InsertRun(repo.ID, "sentinel", sha, sha)
			if err != nil {
				t.Fatal(err)
			}
			run, err = d.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			repo, err = d.GetRepo(repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"no-mistakes-gate-config", "no-mistakes-shared-config.json", "hooks/pre-receive", "hooks/post-receive"} {
				if err := os.Remove(filepath.Join(bare, name)); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(bare, "hooks/pre-receive"), []byte("#!/bin/sh\nexit 7\n"), 0o751); err != nil {
				t.Fatal(err)
			}
			if _, err := git.RunBare(ctx, bare, "config", "receive.advertisePushOptions", "false"); err != nil {
				t.Fatal(err)
			}
			control := openControlDB(t, p.DB())
			if _, err := control.Exec("CREATE TRIGGER refuse_metadata_update BEFORE UPDATE ON repos BEGIN SELECT RAISE(ABORT, 'metadata UPDATE refusal'); END"); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(root, "owner-alias")
			if err := os.Symlink(p.Root(), alias); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			if err := listener.(*net.TCPListener).SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
				t.Fatal(err)
			}
			start := func(mode, home, barrier string, wait time.Duration) (*exec.Cmd, *bytes.Buffer) {
				t.Helper()
				encoded, err := json.Marshal(metadataProcessInput{home, work, mode, wait})
				if err != nil {
					t.Fatal(err)
				}
				cmd := exec.CommandContext(ctx, exe, "-test.run=^TestGateMetadataProcessEntry$")
				cmd.Env = append(os.Environ(), "NM160_METADATA_PROCESS="+string(encoded), "NM160_REAL_GIT="+realGit, "NM160_WRITE_BARRIER="+barrier, "PATH="+helperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
				output := new(bytes.Buffer)
				cmd.Stdout, cmd.Stderr = output, output
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				return cmd, output
			}
			finish := func(cmd *exec.Cmd, output *bytes.Buffer) metadataProcessResult {
				t.Helper()
				if err := cmd.Wait(); err != nil {
					t.Fatalf("fixture process: %v\n%s", err, output)
				}
				for _, line := range strings.Split(output.String(), "\n") {
					if data, ok := strings.CutPrefix(line, "RESULT "); ok {
						var result metadataProcessResult
						if err := json.Unmarshal([]byte(data), &result); err != nil {
							t.Fatal(err)
						}
						return result
					}
				}
				t.Fatalf("missing fixture result: %s", output)
				return metadataProcessResult{}
			}
			before := metadataGateTree(t, bare)
			firstCmd, firstOutput := start(first, p.Root(), listener.Addr().String(), 30*time.Second)
			waited := false
			defer func() {
				if !waited {
					if err := firstCmd.Process.Kill(); err != nil {
						t.Error(err)
					}
					if err := firstCmd.Wait(); err == nil {
						t.Error("unfinished first process unexpectedly succeeded")
					}
				}
			}()
			conn, err := listener.Accept()
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadFull(conn, make([]byte, 1)); err != nil {
				t.Fatal(err)
			}
			mid := metadataGateTree(t, bare)
			second := "migrate"
			if first == "migrate" {
				second = "init"
			}
			secondCmd, secondOutput := start(second, alias, "", time.Second)
			secondResult := finish(secondCmd, secondOutput)
			postSecond := metadataGateTree(t, bare)
			if _, err := conn.Write([]byte{1}); err != nil {
				t.Fatal(err)
			}
			firstResult := finish(firstCmd, firstOutput)
			waited = true
			t.Logf("OS process order %s -> %s: first=%+v second=%+v", first, second, firstResult, secondResult)
			if first == "init" {
				if !strings.Contains(firstResult.Failure, "metadata UPDATE refusal") || firstResult.Created {
					t.Fatalf("wrong init refusal: %+v", firstResult)
				}
				want := before
				if secondResult.Stats.Migrated == 1 {
					want = postSecond
				}
				if !reflect.DeepEqual(want, metadataGateTree(t, bare)) {
					t.Error("failed refresh overwrote completed migration or failed to restore the whole gate")
				}
				if secondResult.Stats.Failed != 1 || secondResult.Stats.Migrated != 0 {
					t.Errorf("migration did not refuse a held enrollment lock: %+v", secondResult)
				}
			} else {
				if firstResult.Stats.Migrated != 1 || firstResult.Stats.Failed != 0 {
					t.Fatalf("first migration failed: %+v", firstResult)
				}
				if !strings.Contains(secondResult.Failure, "context deadline exceeded") {
					t.Errorf("init did not refuse a held migration lock: %+v", secondResult)
				}
			}
			if !reflect.DeepEqual(mid, postSecond) {
				t.Error("contending writer changed the gate before the owner released it")
			}
			if stats := migrateGateConfigs(ctx, d, p); stats.Failed != 0 || stats.Gates != 1 || !git.GateConfigCurrent(bare) {
				t.Fatalf("migration retry failed or leaked lock: %+v", stats)
			}
			migrated := metadataGateTree(t, bare)
			if _, _, err := gate.Init(ctx, d, p, work); err == nil || !strings.Contains(err.Error(), "metadata UPDATE refusal") {
				t.Fatalf("retry did not reach SQLite UPDATE: %v", err)
			}
			if !reflect.DeepEqual(migrated, metadataGateTree(t, bare)) {
				t.Error("refresh refusal rewrote completed migration")
			}
			if _, err := control.Exec("DROP TRIGGER refuse_metadata_update"); err != nil {
				t.Fatal(err)
			}
			if _, created, err := gate.Init(ctx, d, p, work); err != nil || created {
				t.Fatalf("successful retry failed or claimed ownership: %v %v", created, err)
			}
			if got, err := d.GetRepo(repo.ID); err != nil || !reflect.DeepEqual(got, repo) {
				t.Fatalf("registration changed: %+v %v", got, err)
			}
			if got, err := d.GetRun(run.ID); err != nil || !reflect.DeepEqual(got, run) {
				t.Fatalf("seeded run changed: %+v %v", got, err)
			}
			if got, err := git.HeadSHA(ctx, linked); err != nil || got != sha {
				t.Fatalf("gate linked worktree lost sentinel: %s %v", got, err)
			}
		})
	}
}
