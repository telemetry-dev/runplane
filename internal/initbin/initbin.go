// Package initbin holds the runplane-init binary for the architecture of
// this build. `make init` builds it, gzipped, into bin/runplane-init before
// the runplane build; without it, bin has only a placeholder.
package initbin

import (
	"bytes"
	"compress/gzip"
	"embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

//go:embed all:bin
var bin embed.FS

// gzipped is the embedded binary, or empty when the build has none.
var gzipped, _ = bin.ReadFile("bin/runplane-init")

// Bytes returns the runplane-init binary: the embedded one, or else
// runplane-init next to the runplane executable.
func Bytes() ([]byte, error) {
	if len(gzipped) == 0 {
		p, err := besideExecutable()
		if err != nil {
			return nil, err
		}
		return os.ReadFile(p)
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

// Path returns the path of a runplane-init binary: the embedded binary,
// written one time to a temporary file, or runplane-init next to the
// runplane executable.
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
	if len(gzipped) == 0 {
		return besideExecutable()
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

func besideExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("find runplane-init: %w", err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", fmt.Errorf("find runplane-init: %w", err)
	}
	p := filepath.Join(filepath.Dir(exe), "runplane-init")
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() {
		return "", errors.New("find runplane-init: this build has no embedded runplane-init and there is none next to the runplane binary; build it with make init")
	}
	return p, nil
}
