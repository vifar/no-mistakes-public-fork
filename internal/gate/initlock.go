package gate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// initLockWait bounds how long init, eject, or startup gate migration waits
// for ownership before giving up. It is a variable so a test can shorten it.
var initLockWait = 2 * time.Minute

// initLockPoll is how often a waiting ownership operation retries the lock.
const initLockPoll = 25 * time.Millisecond

// initLockPath is shared by init, eject, and startup gate migration for this
// no-mistakes root so ownership decisions and metadata rollback cannot overlap
// another cooperating writer's changes.
func initLockPath(p *paths.Paths) string {
	return filepath.Join(p.Root(), "init.lock")
}

// AcquireInitLock takes the exclusive init lock of a no-mistakes root.
//
// The repo ID a checkout derives names a gate directory that is shared by
// everything its registration owns. Deciding that an ID is free, creating its
// gate, registering it and rolling a failure back are separate steps over
// the registry and the filesystem; a second init (in this process or another)
// interleaving with them could see the same ID as free, or remove a gate the
// other one has just registered (issue #160). The lock makes the whole
// sequence one exclusive decision: it is held from the first registry read
// until the attempt has registered or rolled back. Startup gate migration
// holds the same lock from candidate discovery through the full migration pass,
// preventing init rollback from undoing a completed migration. Callers acquire
// it once around the operation, not again inside individual metadata writers.
//
// The lock is an OS file lock, so it is released when the holder exits, even
// on a crash.
func AcquireInitLock(ctx context.Context, p *paths.Paths) (func(), error) {
	path := initLockPath(p)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create init lock directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open init lock: %w", err)
	}
	deadline := time.Now().Add(initLockWait)
	for {
		lockErr := tryLockInitFile(f)
		if lockErr == nil {
			return func() {
				_ = unlockInitFile(f)
				_ = f.Close()
			}, nil
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("another no-mistakes init or eject still holds %s after %s; wait for it to finish and retry: %w", path, initLockWait, lockErr)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(initLockPoll):
		}
	}
}
