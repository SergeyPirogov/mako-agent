// Package fsx writes Mako's files so that a crash or a full disk never leaves one half written.
package fsx

import (
	"errors"
	"os"
	"path/filepath"
)

// WriteFile replaces path with data at once: it is written to a temporary file in the same folder, synced and
// renamed over the old one, so a reader sees either the old content or the new one. The file gets perm even
// when it was there with other rights (a .env left 0644). On an error the old file stays as it was.
func WriteFile(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Chmod(perm); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	// The rename itself is safe on disk only once the folder is synced; a folder that cannot be is no failure.
	if d, e := os.Open(dir); e == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// Rotate moves path to path.1 (replacing an older one) when it grew past max bytes, so a log does not grow forever.
func Rotate(path string, max int64) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) || err == nil && info.Size() <= max {
		return nil
	}
	if err != nil {
		return err
	}
	return os.Rename(path, path+".1")
}

// ErrLocked: another process holds the lock.
var ErrLocked = errors.New("locked by another process")
