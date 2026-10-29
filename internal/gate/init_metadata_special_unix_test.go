//go:build unix

package gate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestInitComponentOpaqueMetadata(t *testing.T) {
	for _, name := range []string{"hooks/pre-receive.no-mistakes-user", "no-mistakes-gate-config", "no-mistakes-shared-config.json"} {
		for _, refuse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/refuse=%v", name, refuse), func(t *testing.T) {
				ctx := context.Background()
				p, d := newInitFixture(t)
				work := setupTestRepo(t)
				repo, _, err := Init(ctx, d, p, work)
				if err != nil {
					t.Fatal(err)
				}
				bare := p.RepoDir(repo.ID)
				path := filepath.Join(bare, name)
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				if err := syscall.Mkfifo(path, 0o600); err != nil {
					t.Fatal(err)
				}
				identity, err := os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				before := treeSnapshot(t, bare)
				if refuse {
					raw, err := sql.Open("sqlite", p.DB())
					if err != nil {
						t.Fatal(err)
					}
					_, writeErr := raw.Exec("CREATE TRIGGER refuse_opaque_init BEFORE UPDATE ON repos BEGIN SELECT RAISE(ABORT, 'opaque SQLite refusal'); END")
					if err := errors.Join(writeErr, raw.Close()); err != nil {
						t.Fatal(err)
					}
				}
				_, _, err = Init(ctx, d, p, work)
				if name == "hooks/pre-receive.no-mistakes-user" {
					if refuse {
						if err == nil || !strings.Contains(err.Error(), "opaque SQLite refusal") {
							t.Fatalf("untouched FIFO prevented refresh reaching SQLite: %v", err)
						}
					} else if err != nil {
						t.Fatalf("untouched FIFO prevented refresh: %v", err)
					}
				} else if err == nil || !strings.Contains(err.Error(), "refuse to replace opaque gate metadata") {
					t.Fatalf("attempt overwrote an opaque entry: %v", err)
				}
				compareSnapshots(t, "unowned FIFO", before, treeSnapshot(t, bare))
				after, err := os.Lstat(path)
				if err != nil || !os.SameFile(identity, after) {
					t.Fatalf("unowned FIFO identity changed: %v", err)
				}
			})
		}
	}
}
