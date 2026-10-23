//go:build unix

package gate

import (
	"os"
	"syscall"
)

// tryLockInitFile takes a non-blocking exclusive flock on f. flock belongs to
// the open file description, so two opens of the lock file in one process
// exclude each other just as two processes do.
func tryLockInitFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func unlockInitFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
