// Package ext4test examines the ext4 files of the tests. Its checks need
// e2fsprogs; without it they skip the test, so callers run them in a
// subtest of their own.
package ext4test

import (
	"encoding/binary"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Check examines the ext4 superblock magic, and the file system with
// e2fsck. The hcsshim writer does not set the padding bits of the inode
// bitmap. The kernel does not read them on a read-only mount, so the check
// permits that one problem.
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
	out, err := exec.Command(tool(t, "e2fsck"), "-fn", file).CombinedOutput()
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
// output has want.
func Debugfs(t *testing.T, file, request, want string) {
	t.Helper()
	out, err := exec.Command(tool(t, "debugfs"), "-R", request, file).CombinedOutput()
	if err != nil {
		t.Fatalf("debugfs -R %q: %v\n%s", request, err, out)
	}
	if !strings.Contains(string(out), want) {
		t.Errorf("debugfs -R %q:\n%s\nwant %q", request, out, want)
	}
}

// tool returns the path of a program of e2fsprogs, or skips the test.
func tool(t *testing.T, name string) string {
	t.Helper()
	bin, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("needs %s of e2fsprogs to read the ext4 file", name)
	}
	return bin
}
