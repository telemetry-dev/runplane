// Package initbin holds the runplane-init binary for the architecture of
// this build. `make init` builds it, gzipped, into File before the runplane
// build; without it, bin has only a placeholder.
package initbin

import (
	"bytes"
	"compress/gzip"
	"embed"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// File is the gzipped binary that `make init` writes, relative to this
// package.
const File = "bin/runplane-init"

//go:embed all:bin
var bin embed.FS

// gzipped is the embedded binary, or empty when the build has none.
var gzipped, _ = bin.ReadFile(File)

var errNoInit = errors.New("this build of runplane has no runplane-init: run make init, then build runplane again")

// Bytes returns the embedded runplane-init binary.
func Bytes() ([]byte, error) {
	if len(gzipped) == 0 {
		return nil, errNoInit
	}
	zr, err := gzip.NewReader(bytes.NewReader(gzipped))
	if err != nil {
		return nil, fmt.Errorf("read runplane-init: %w", err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("read runplane-init: %w", err)
	}
	return b, nil
}

var (
	mu   sync.Mutex
	path string
)

// Path returns the path of the embedded runplane-init binary, which it
// writes one time to a temporary file.
func Path() (string, error) {
	mu.Lock()
	defer mu.Unlock()
	// A cleaner of the temporary folder can remove the file while runplane
	// operates, so write it again when it is gone.
	if path != "" {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	b, err := Bytes()
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "runplane-init-*")
	if err != nil {
		return "", fmt.Errorf("write runplane-init: %w", err)
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(f.Name(), 0o755)
	}
	if err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("write runplane-init: %w", err)
	}
	path = f.Name()
	return path, nil
}
