package gate

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// initMetadata journals configureGate's writable metadata while init.lock is held.
// It deliberately excludes objects, refs and linked-worktree administration.
// The whole-gate failure oracle must catch any future configureGate write added
// outside this set. This is returned-error rollback, not crash recovery.
type initMetadata struct {
	files        []initMetadataFile
	hooksDir     string
	hooksCreated bool
}

type initMetadataFile struct {
	path   string
	exists bool
	mode   os.FileMode
	data   []byte
}

func snapshotInitMetadata(bareDir string) (*initMetadata, error) {
	s := &initMetadata{hooksDir: filepath.Join(bareDir, "hooks")}
	info, err := os.Lstat(s.hooksDir)
	if os.IsNotExist(err) {
		s.hooksCreated = true
	} else if err != nil {
		return nil, err
	} else if !info.IsDir() {
		return nil, fmt.Errorf("gate hooks path must be a directory: %s", s.hooksDir)
	}
	// Include the companion created when preserving a custom pre-receive hook.
	for _, name := range []string{"config", "config.worktree", "hooks/pre-receive", "hooks/pre-receive.no-mistakes-user", "hooks/post-receive", "no-mistakes-gate-config", "no-mistakes-shared-config.json"} {
		f := initMetadataFile{path: filepath.Join(bareDir, filepath.FromSlash(name))}
		info, err := os.Lstat(f.path)
		if os.IsNotExist(err) {
			s.files = append(s.files, f)
			continue
		}
		if err != nil {
			return nil, err
		}
		// Refuse indirection and special files BEFORE any provisioning write.
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("gate metadata must be a regular file: %s", f.path)
		}
		f.exists = true
		f.mode = info.Mode()
		f.data, err = os.ReadFile(f.path)
		if err != nil {
			return nil, err
		}
		s.files = append(s.files, f)
	}
	return s, nil
}

func (s *initMetadata) restore() error {
	var result error
	for _, f := range s.files {
		if err := f.restore(); err != nil {
			result = errors.Join(result, fmt.Errorf("restore gate metadata %s: %w", f.path, err))
		}
	}
	if s.hooksCreated {
		// Never recursively remove a directory: another user may have added a file.
		if err := os.Remove(s.hooksDir); err != nil && !os.IsNotExist(err) {
			result = errors.Join(result, fmt.Errorf("remove init-created hooks directory: %w", err))
		}
	}
	return result
}

func (f initMetadataFile) restore() (result error) {
	info, err := os.Lstat(f.path)
	if os.IsNotExist(err) {
		if !f.exists {
			return nil
		}
	} else if err != nil {
		return err
	} else {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("metadata type changed during init; refusing replacement")
		}
		if !f.exists {
			return os.Remove(f.path)
		}
		data, err := os.ReadFile(f.path)
		if err != nil {
			return err
		}
		if info.Mode() == f.mode && bytes.Equal(data, f.data) {
			return nil
		}
	}
	// Restore atomically so readers cannot observe a truncated config or hook.
	tmp, err := os.CreateTemp(filepath.Dir(f.path), ".init-rollback-*")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.Remove(tmp.Name()); err != nil && !os.IsNotExist(err) {
			result = errors.Join(result, err)
		}
	}()
	if err := tmp.Chmod(f.mode); err != nil {
		return errors.Join(err, tmp.Close())
	}
	_, writeErr := tmp.Write(f.data)
	if err := errors.Join(writeErr, tmp.Close()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), f.path)
}
