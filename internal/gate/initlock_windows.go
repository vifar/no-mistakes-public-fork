//go:build windows

package gate

import (
	"os"

	"golang.org/x/sys/windows"
)

// tryLockInitFile takes a non-blocking exclusive lock on the first byte of f
// via LockFileEx. Like flock on Unix, the OS releases it when the owning
// handle is closed or the process exits.
func tryLockInitFile(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
}

func unlockInitFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{})
}
