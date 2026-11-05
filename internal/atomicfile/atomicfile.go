// Package atomicfile replaces files so that a reader or a crash never sees
// half a file.
package atomicfile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Write copies r to a temporary file in the folder of path, syncs it, and
// renames it to path.
func Write(path string, r io.Reader, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())
	_, err = io.Copy(tmp, r)
	if err = errors.Join(err, tmp.Chmod(mode), tmp.Sync(), tmp.Close()); err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
