//go:build unix

package branchsync

import (
	"errors"
	"os"
	"syscall"
)

func tryLockCustodyRecoveryFile(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return errCustodyRecoveryBusy
	}
	return err
}

func unlockCustodyRecoveryFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
