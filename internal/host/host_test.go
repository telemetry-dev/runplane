package host

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func open(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestParseOSRelease(t *testing.T) {
	tests := []struct {
		file                  string
		pretty, id, versionID string
		name                  string
	}{
		{"ubuntu-24.04", "Ubuntu 24.04.3 LTS", "ubuntu", "24.04", "Ubuntu"},
		{"debian-12", "Debian GNU/Linux 12 (bookworm)", "debian", "12", "Debian GNU/Linux"},
		{"custom", "Example OS 1.0 (Server)", "example", "1.0", `Say "hi"`},
	}
	for _, tt := range tests {
		r := parseOSRelease(open(t, "testdata/os-release/"+tt.file))
		got := []string{r["PRETTY_NAME"], r["ID"], r["VERSION_ID"], r["NAME"]}
		if want := []string{tt.pretty, tt.id, tt.versionID, tt.name}; !slices.Equal(got, want) {
			t.Errorf("%s: got %q, want %q", tt.file, got, want)
		}
	}
}

func TestParseMeminfo(t *testing.T) {
	tests := []struct {
		file             string
		total, available uint64
	}{
		{"meminfo", 16422380, 13449000},
		// No MemAvailable before Linux 3.14: free + buffers + cached.
		{"meminfo-3.10", 4046860, 1500000},
	}
	for _, tt := range tests {
		m, err := parseMeminfo(open(t, "testdata/proc/"+tt.file))
		if err != nil {
			t.Fatal(err)
		}
		if m.totalKB != tt.total || m.availableKB != tt.available {
			t.Errorf("%s: got %+v, want total %d available %d", tt.file, m, tt.total, tt.available)
		}
	}
	if _, err := parseMeminfo(strings.NewReader("MemFree: 1 kB\n")); err == nil {
		t.Error("meminfo without MemTotal: want an error")
	}
}

func TestDefaultInterface(t *testing.T) {
	// Two default routes: eth1 has the lower metric. wg0 is down.
	if got := defaultInterface(open(t, "testdata/proc/net-route")); got != "eth1" {
		t.Errorf("got %q, want eth1", got)
	}
}

func TestBlockKind(t *testing.T) {
	sys := t.TempDir()
	devices := filepath.Join(sys, "devices")
	write := func(path, value string) {
		path = filepath.Join(devices, path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := func(dir, name, target string) {
		if err := os.MkdirAll(filepath.Join(sys, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(devices, target), filepath.Join(sys, dir, name)); err != nil {
			t.Fatal(err)
		}
	}

	// An NVMe partition.
	write("nvme0n1/nvme0n1p1/partition", "1")
	link("dev/block", "259:1", "nvme0n1/nvme0n1p1")

	// LVM on a partition of a SATA SSD.
	write("sda/queue/rotational", "0")
	write("sda/sda2/partition", "2")
	link("class/block", "sda2", "sda/sda2")
	if err := os.MkdirAll(filepath.Join(devices, "dm-0/slaves/sda2"), 0o755); err != nil {
		t.Fatal(err)
	}
	link("dev/block", "253:0", "dm-0")

	// A rotational disk.
	write("sdb/queue/rotational", "1")
	link("dev/block", "8:16", "sdb")

	tests := []struct {
		dev     string
		virtual bool
		want    string
	}{
		{"259:1", false, "NVMe"},
		{"253:0", false, "SSD"},
		{"8:16", false, "HDD"},
		{"8:16", true, ""}, // hypervisors mark virtual disks as rotational
		{"7:0", false, ""}, // no such device
	}
	for _, tt := range tests {
		if got := blockKind(sys, filepath.Join(sys, "dev/block", tt.dev), tt.virtual); got != tt.want {
			t.Errorf("%s virtual=%v: got %q, want %q", tt.dev, tt.virtual, got, tt.want)
		}
	}
}

func TestAtLeast(t *testing.T) {
	tests := []struct {
		have, want string
		ok         bool
	}{
		{"29.5.0", "29.5", true},
		{"29.10.1", "29.5", true}, // numbers, not text
		{"28.5.2", "29.5", false},
		{"30.0.0", "29.5", true},
		{"0.7.0", "0.7.1", false},
		{"0.7.1", "0.7.1", true},
		{"0.7.1-0.linux", "0.7.1", true},
		{"0.10.0", "0.7.1", true},
		{"0.6.7+dev", "0.7.1", false},
		{"v1.13.1", "1.13", true},
		{"6.8.0-48-generic", "5.12", true},
		{"5.4.0-200-generic", "5.12", false},
		{"", "0.7.1", false},
		{"unknown", "0.7.1", false},
		{"29", "29.5", false}, // a major number alone is not a version
	}
	for _, tt := range tests {
		if got := atLeast(tt.have, tt.want); got != tt.ok {
			t.Errorf("atLeast(%q, %q) = %v, want %v", tt.have, tt.want, got, tt.ok)
		}
	}
}

func TestFirstVersion(t *testing.T) {
	for file, want := range map[string]string{
		"firecracker": "1.13.1",
		"jailer":      "1.13.1",
		"sysbox-runc": "0.7.1",
	} {
		b, err := os.ReadFile("testdata/tools/" + file)
		if err != nil {
			t.Fatal(err)
		}
		if got := firstVersion(string(b)); got != want {
			t.Errorf("%s: got %q, want %q", file, got, want)
		}
	}
}
