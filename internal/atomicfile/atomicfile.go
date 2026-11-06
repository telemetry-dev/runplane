// Package atomicfile replaces files so that a reader or a crash never sees
// half a file.
package atomicfile

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Write copies r to a temporary file in the folder of path, syncs it, and
// renames it to path.
func Write(path string, r io.Reader, mode os.FileMode) error {
	f, err := Create(path, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return f.Commit()
}

// CreateOnce writes data to a new file at path with mode, and syncs it. If a
// file is at path, for example because another process made it first, it
// returns an error that wraps fs.ErrExist and keeps that file.
func CreateOnce(path string, data []byte, mode os.FileMode) error {
	f, err := Create(path, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	// A hard link puts the complete file in place. The link is not
	// successful if a file is there. Close removes the temporary name.
	if err := os.Link(f.Name(), path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// File is a temporary file that replaces the file at its path on Commit.
type File struct {
	*os.File
	path      string
	committed bool
}

// Create makes the temporary file for path in the folder of path.
func Create(path string, mode os.FileMode) (*File, error) {
	return CreateIn(filepath.Dir(path), path, mode)
}

// CreateIn makes the temporary file for path in dir, which must be on the
// file system of path.
func CreateIn(dir, path string, mode os.FileMode) (*File, error) {
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return nil, fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, fmt.Errorf("write %s: %w", path, err)
	}
	return &File{File: f, path: path}, nil
}

// Commit syncs the file and renames it to its path. The file stays open.
func (f *File) Commit() error {
	err := f.Sync()
	if err == nil {
		err = os.Rename(f.Name(), f.path)
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", f.path, err)
	}
	f.committed = true
	return nil
}

// Before a Commit, Close also removes the file.
func (f *File) Close() error {
	err := f.File.Close()
	if !f.committed {
		os.Remove(f.Name())
	}
	return err
}
