package ext4

import (
	"archive/tar"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteLeavesNothingOnError(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "disk.ext4")
	_, err := Write(dst, func(w io.Writer) error {
		tw := tar.NewWriter(w)
		tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "a", Size: 1, Mode: 0o644})
		tw.Write([]byte("a"))
		return errors.New("network down")
	})
	if err == nil || !strings.Contains(err.Error(), "network down") {
		t.Fatalf("Write error = %v, want the fill error", err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("folder has %v after a failure, want nothing", left)
	}
}
