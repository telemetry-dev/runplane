package firecracker

import (
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func TestJailerIDs(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	subuid, subgid := filepath.Join(dir, "subuid"), filepath.Join(dir, "subgid")
	writeFile(t, subuid, "ubuntu:100000:65536\n"+me.Username+":1879048192:16384\n")
	// The gid file names the user by its uid and has a smaller range.
	writeFile(t, subgid, "ubuntu:100000:65536\n"+me.Uid+":1879000000:8\n")

	r, err := readIDRange(me.Username, subuid, subgid)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ index, uid, gid int }{{0, 1879048192, 1879000000}, {7, 1879048199, 1879000007}} {
		uid, gid, err := r.of(tt.index)
		if err != nil || uid != tt.uid || gid != tt.gid {
			t.Errorf("lease %d: %d:%d, %v; want %d:%d", tt.index, uid, gid, err, tt.uid, tt.gid)
		}
	}
	// Two VMs must never share IDs, so a lease past the smaller range fails.
	if _, _, err := r.of(8); err == nil {
		t.Error("lease 8 got IDs from a gid range of 8")
	}

	for name, content := range map[string]string{
		"no range for the user": "ubuntu:100000:65536\n",
		"a range from root":     me.Username + ":0:65536\n",
		"a count of 0":          me.Username + ":1879048192:0\n",
	} {
		writeFile(t, subgid, content)
		if _, err := readIDRange(me.Username, subuid, subgid); err == nil || !strings.Contains(err.Error(), subgid) {
			t.Errorf("%s: got %v, want an error about %s", name, err, subgid)
		}
	}
}
