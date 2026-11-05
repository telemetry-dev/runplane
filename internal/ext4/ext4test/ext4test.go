// Package ext4test examines the ext4 files of the tests.
package ext4test

import (
	"encoding/binary"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Check examines the ext4 superblock magic, and the file system with
// e2fsck where e2fsprogs is installed. The hcsshim writer does not set the
// padding bits of the inode bitmap. The kernel does not read them on a
// read-only mount, so the check permits that one problem.
func Check(t *testing.T, file string) {
	t.Helper()
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	magic := make([]byte, 2)
	if _, err := f.ReadAt(magic, 1080); err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint16(magic); got != 0xef53 {
		t.Fatalf("magic at 1080 = %#x, want 0xef53", got)
	}
	bin, err := exec.LookPath("e2fsck")
	if err != nil {
		t.Log("no e2fsck, skip the file system check")
		return
	}
	out, err := exec.Command(bin, "-fn", file).CombinedOutput()
	if exit, ok := err.(*exec.ExitError); err != nil && (!ok || exit.ExitCode() != 4) {
		t.Fatalf("e2fsck -fn %s: %v\n%s", file, err, out)
	}
	for line := range strings.Lines(string(out)) {
		if strings.HasSuffix(strings.TrimSpace(line), "? no") && !strings.Contains(line, "Padding at end of inode bitmap is not set") {
			t.Errorf("e2fsck -fn %s:\n%s", file, out)
			return
		}
	}
}

// Debugfs runs a debugfs request on an ext4 file and examines that the
// output has want. It skips the check where e2fsprogs is not installed.
func Debugfs(t *testing.T, file, request, want string) {
	t.Helper()
	bin, err := exec.LookPath("debugfs")
	if err != nil {
		t.Logf("no debugfs, skip %q", request)
		return
	}
	out, err := exec.Command(bin, "-R", request, file).CombinedOutput()
	if err != nil {
		t.Fatalf("debugfs -R %q: %v\n%s", request, err, out)
	}
	if !strings.Contains(string(out), want) {
		t.Errorf("debugfs -R %q:\n%s\nwant %q", request, out, want)
	}
}
