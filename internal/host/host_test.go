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
		t.Run(tt.file, func(t *testing.T) {
			r := parseOSRelease(open(t, "testdata/os-release/"+tt.file))
			got := []string{r["PRETTY_NAME"], r["ID"], r["VERSION_ID"], r["NAME"]}
			want := []string{tt.pretty, tt.id, tt.versionID, tt.name}
			if !slices.Equal(got, want) {
				t.Errorf("got %q, want %q", got, want)
			}
		})
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

func TestListenInodes(t *testing.T) {
	// Port 8484 (0x2124) has a listening socket and an established one.
	got := listenInodes(open(t, "testdata/proc/net-tcp"), 8484)
	if !slices.Equal(got, []string{"44817"}) {
		t.Errorf("got %v, want [44817]", got)
	}
}

func TestBlockKind(t *testing.T) {
	sys := t.TempDir()
	devices := filepath.Join(sys, "devices")
	mkdir := func(path string) {
		if err := os.MkdirAll(filepath.Join(devices, path), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, value string) {
		if err := os.WriteFile(filepath.Join(devices, path), []byte(value+"\n"), 0o644); err != nil {
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
	mkdir("nvme0n1/nvme0n1p1")
	write("nvme0n1/nvme0n1p1/partition", "1")
	link("dev/block", "259:1", "nvme0n1/nvme0n1p1")

	// LVM on a partition of a SATA SSD.
	mkdir("sda/sda2")
	mkdir("sda/queue")
	write("sda/queue/rotational", "0")
	write("sda/sda2/partition", "2")
	link("class/block", "sda2", "sda/sda2")
	mkdir("dm-0/slaves/sda2")
	link("dev/block", "253:0", "dm-0")

	// A rotational disk.
	mkdir("sdb/queue")
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

func TestParseIptables(t *testing.T) {
	tests := []struct {
		file       string
		policy     string
		dockerUser bool
	}{
		{"docker", "DROP", true},
		{"ufw", "DROP", false},
		{"plain", "ACCEPT", false},
	}
	for _, tt := range tests {
		b, err := os.ReadFile("testdata/iptables/" + tt.file)
		if err != nil {
			t.Fatal(err)
		}
		policy, dockerUser := parseIptables(string(b))
		if policy != tt.policy || dockerUser != tt.dockerUser {
			t.Errorf("%s: got %q %v, want %q %v", tt.file, policy, dockerUser, tt.policy, tt.dockerUser)
		}
	}
}
