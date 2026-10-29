package gate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// initLockWait bounds how long an init (or eject) waits for another one to
// finish before giving up. It is a variable so a test can shorten it.
var initLockWait = 2 * time.Minute

// initLockPoll is how often a waiting init retries the lock.
const initLockPoll = 25 * time.Millisecond

// initLockPath is the lock every init and eject of this no-mistakes root
// holds while it decides who owns a repo ID and acts on that decision.
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
// until the attempt has registered or rolled back.
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
