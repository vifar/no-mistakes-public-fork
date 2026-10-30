package branchsync

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var errCustodyRecoveryBusy = errors.New("custody recovery lock is held")

// The OS releases this lock on close or process exit, so a crash leaves no stale lock.
func (s *Service) acquireCustodyRecoveryLock() (*os.File, *State) {
	gateDir := strings.TrimSpace(s.GateDir)
	if gateDir == "" {
		return nil, nil
	}
	f, err := os.OpenFile(filepath.Join(gateDir, "no-mistakes-custody-recovery.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err == nil {
		if err = tryLockCustodyRecoveryFile(f); err == nil {
			return f, nil
		}
		err = errors.Join(err, f.Close())
	}
	state := State{State: StatePipelineOwned, Safety: "blocked_recover_preserve_failed", Error: fmt.Sprintf("the custody recovery lock could not be acquired: %v; custody was not returned", err)}
	if errors.Is(err, errCustodyRecoveryBusy) {
		state.Safety = "blocked_recover_in_progress"
		state.Error = "another custody recovery is running on this repository; re-run after it finishes; no files or refs were changed"
	}
	return nil, &state
}

func releaseCustodyRecoveryLock(f *os.File, result *State) {
	if f == nil {
		return
	}
	if err := errors.Join(unlockCustodyRecoveryFile(f), f.Close()); err != nil {
		result.Safety = "blocked_recover_lock_failed"
		result.Error += fmt.Sprintf("; the custody recovery lock could not be released: %v", err)
		result.Recovered = false
	}
}
