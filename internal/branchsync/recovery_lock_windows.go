//go:build windows

package branchsync

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func tryLockCustodyRecoveryFile(f *os.File) error {
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return errCustodyRecoveryBusy
	}
	return err
}

func unlockCustodyRecoveryFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{})
}
