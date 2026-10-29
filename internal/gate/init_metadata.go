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
	info   os.FileInfo
}

func snapshotInitMetadata(bareDir string) (*initMetadata, error) {
	s := &initMetadata{hooksDir: filepath.Join(bareDir, "hooks")}
	info, err := os.Lstat(s.hooksDir)
	if os.IsNotExist(err) {
		s.hooksCreated = true
	} else if err != nil {
		return nil, err
	} else {
		if info.Mode()&os.ModeSymlink != 0 {
			info, err = os.Stat(s.hooksDir)
			if err != nil {
				return nil, err
			}
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("gate hooks path must be a directory: %s", s.hooksDir)
		}
		s.hooksDir, err = filepath.EvalSymlinks(s.hooksDir)
		if err != nil {
			return nil, err
		}
	}
	// Include the companion created when preserving a custom pre-receive hook.
	for _, name := range []string{"config", "config.worktree", "hooks/pre-receive", "hooks/pre-receive.no-mistakes-user", "hooks/post-receive", "no-mistakes-gate-config", "no-mistakes-shared-config.json"} {
		path := filepath.Join(bareDir, filepath.FromSlash(name))
		seen := make(map[string]bool)
		for {
			parent, err := filepath.EvalSymlinks(filepath.Dir(path))
			if err != nil && !os.IsNotExist(err) {
				return nil, err
			}
			if err == nil {
				path = filepath.Join(parent, filepath.Base(path))
			}
			if seen[path] {
				return nil, fmt.Errorf("gate metadata symlink cycle: %s", path)
			}
			seen[path] = true
			f := initMetadataFile{path: path}
			info, err := os.Lstat(path)
			if os.IsNotExist(err) {
				s.files = append(s.files, f)
				break
			}
			if err != nil {
				return nil, err
			}
			f.exists, f.mode, f.info = true, info.Mode(), info
			switch {
			case f.mode.IsRegular():
				f.data, err = os.ReadFile(path)
			case f.mode&os.ModeSymlink != 0:
				var target string
				target, err = os.Readlink(path)
				f.data = []byte(target)
			}
			if err != nil {
				return nil, err
			}
			s.files = append(s.files, f)
			if f.mode&os.ModeSymlink == 0 || (name != "config" && name != "config.worktree") {
				break
			}
			path = string(f.data)
			if !filepath.IsAbs(path) {
				path = filepath.Join(filepath.Dir(f.path), path)
			}
			path = filepath.Clean(path)
		}
	}
	return s, nil
}

func (s *initMetadata) restore() error {
	var result error
	var preReceive, companion *initMetadataFile
	for i := range s.files {
		f := &s.files[i]
		switch f.path {
		case filepath.Join(s.hooksDir, "pre-receive"):
			preReceive = f
		case filepath.Join(s.hooksDir, "pre-receive.no-mistakes-user"):
			companion = f
		}
	}
	if preReceive != nil && preReceive.exists && companion != nil && !companion.exists {
		info, err := os.Lstat(companion.path)
		if err == nil && os.SameFile(preReceive.info, info) {
			if err := os.Rename(companion.path, preReceive.path); err != nil {
				result = errors.Join(result, fmt.Errorf("restore preserved pre-receive entry: %w", err))
			}
		} else if err != nil && !os.IsNotExist(err) {
			result = errors.Join(result, fmt.Errorf("inspect preserved pre-receive entry: %w", err))
		}
	}
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
	if f.exists && !f.mode.IsRegular() && f.mode&os.ModeSymlink == 0 {
		if err == nil && info.Mode() == f.mode && os.SameFile(f.info, info) {
			return nil
		}
		return fmt.Errorf("opaque metadata changed during init; refusing replacement")
	}
	if os.IsNotExist(err) {
		if !f.exists {
			return nil
		}
	} else if err != nil {
		return err
	} else {
		if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("metadata type changed during init; refusing replacement")
		}
		if !f.exists {
			return os.Remove(f.path)
		}
		if info.Mode() == f.mode {
			var data []byte
			if f.mode&os.ModeSymlink != 0 {
				var target string
				target, err = os.Readlink(f.path)
				data = []byte(target)
			} else {
				data, err = os.ReadFile(f.path)
			}
			if err != nil {
				return err
			}
			if bytes.Equal(data, f.data) {
				return nil
			}
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
	if f.mode&os.ModeSymlink != 0 {
		if err := errors.Join(tmp.Close(), os.Remove(tmp.Name())); err != nil {
			return err
		}
		if err := os.Symlink(string(f.data), tmp.Name()); err != nil {
			return err
		}
	} else {
		if err := tmp.Chmod(f.mode); err != nil {
			return errors.Join(err, tmp.Close())
		}
		_, writeErr := tmp.Write(f.data)
		if err := errors.Join(writeErr, tmp.Close()); err != nil {
			return err
		}
	}
	return os.Rename(tmp.Name(), f.path)
}
