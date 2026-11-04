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
var files embed.FS

// Bytes returns the embedded runplane-init binary, or nil when the build
// has none.
func Bytes() ([]byte, error) {
	r, err := open()
	if r == nil || err != nil {
		return nil, err
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read runplane-init: %w", err)
	}
	return b, nil
}

// open returns a reader of the embedded binary, or nil when the build has
// none.
func open() (io.Reader, error) {
	b, err := files.ReadFile("bin/runplane-init")
	if err != nil || len(b) == 0 {
		return nil, nil
	}
	return unpack(b)
}

// unpack reads a gzipped binary (make init) or a plain one (go build).
func unpack(b []byte) (io.Reader, error) {
	if !bytes.HasPrefix(b, []byte{0x1f, 0x8b}) {
		return bytes.NewReader(b), nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("read runplane-init: %w", err)
	}
	return zr, nil
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
	r, err := open()
	if err != nil {
		return "", err
	}
	if r == nil {
		return besideExecutable()
	}
	p, err := writeTemp(r)
	if err != nil {
		return "", err
	}
	path = p
	return path, nil
}

func writeTemp(r io.Reader) (string, error) {
	f, err := os.CreateTemp("", "runplane-init-*")
	if err != nil {
		return "", fmt.Errorf("write runplane-init: %w", err)
	}
	_, err = io.Copy(f, r)
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
	return f.Name(), nil
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
