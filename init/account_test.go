package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestLookupAccount(t *testing.T) {
	dir := t.TempDir()
	passwd := filepath.Join(dir, "passwd")
	group := filepath.Join(dir, "group")
	os.WriteFile(passwd, []byte(`root:x:0:0:root:/root:/bin/bash
ubuntu:x:1000:1000:Ubuntu:/home/ubuntu:/bin/bash
runner:x:1001:1001::/home/runner:/bin/bash
`), 0o644)
	os.WriteFile(group, []byte(`root:x:0:
sudo:x:27:ubuntu,runner
docker:x:999:runner
runners:x:1002:runner2
runner:x:1001:runner
`), 0o644)

	a, err := lookupAccount("runner", passwd, group)
	if err != nil {
		t.Fatal(err)
	}
	if a.uid != 1001 || a.gid != 1001 || a.home != "/home/runner" || a.shell != "/bin/bash" {
		t.Errorf("account = %+v", a)
	}
	// Without docker, the runner cannot use the Docker socket.
	if !slices.Equal(a.groups, []uint32{27, 999}) {
		t.Errorf("groups = %v, want [27 999]", a.groups)
	}

	if _, err := lookupAccount("nobody", passwd, group); err == nil {
		t.Error("lookupAccount found a user that is not in passwd")
	}
}
