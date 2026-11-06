package firecracker

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/sandbox/lifecycle"
)

func TestReadUsage(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "cpu.stat"), "usage_usec 6000000\nuser_usec 1\nsystem_usec 1\nnr_periods 0\n")
	writeFile(t, filepath.Join(dir, "memory.current"), "536870912\n")
	writeFile(t, filepath.Join(dir, "memory.stat"), "anon 300000000\nfile 200000000\nactive_file 65782272\ninactive_file 134217728\n")
	u, err := readUsage(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 6 s of CPU time; 512 MiB with 128 MiB of inactive file cache.
	if want := (lifecycle.Usage{CPU: 6 * time.Second, Memory: 536870912, Cache: 134217728}); u != want {
		t.Errorf("usage %+v, want %+v", u, want)
	}
}
