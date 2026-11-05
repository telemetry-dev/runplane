package sandbox

import (
	"net/netip"
	"testing"
)

func TestFindByIP(t *testing.T) {
	list := []Sandbox{
		{ID: "gone", IP: "10.213.0.5", State: StateGone},
		{ID: "bad", IP: "not an address", State: StateIdle},
		{ID: "busy", IP: "10.213.0.5", State: StateBusy},
		{ID: "mapped", IP: "::ffff:10.213.0.9", State: StateIdle},
	}
	for addr, want := range map[string]string{
		"10.213.0.5":        "busy", // a stopped sandbox had the address before
		"::ffff:10.213.0.5": "busy",
		"10.213.0.9":        "mapped",
		"10.213.0.7":        "",
	} {
		got, ok := FindByIP(list, netip.MustParseAddr(addr))
		if got.ID != want || ok != (want != "") {
			t.Errorf("FindByIP(%s) = %q, %v; want %q", addr, got.ID, ok, want)
		}
	}
}

func TestRoom(t *testing.T) {
	server := Capacity{VCPU: 16, MemoryMB: 32768}
	reserve := Reserve{VCPU: 2, MemoryMB: 4096, Overcommit: 1}
	tests := []struct {
		name string
		cap  Capacity
		r    Reserve
		vcpu int
		mem  int
		want int
	}{
		{"vCPUs after the reserve", server, reserve, 4, 1024, 3},
		{"memory after the reserve", server, reserve, 1, 8192, 3},
		{"the overcommit gives vCPUs", server, Reserve{VCPU: 2, MemoryMB: 4096, Overcommit: 2}, 4, 1024, 7},
		{"no overcommit setting", server, Reserve{}, 4, 1024, 4},
		{"the sandboxes that operate", Capacity{VCPU: 16, MemoryMB: 32768, AllocVCPU: 8, AllocMemoryMB: 16384}, Reserve{}, 4, 4096, 2},
		{"the overhead of each sandbox", Capacity{VCPU: 16, MemoryMB: 32768, OverheadMB: 64}, Reserve{}, 1, 8192, 3},
		{"more vCPUs than the driver gives", Capacity{VCPU: 64, MemoryMB: 65536, MaxVCPU: 32}, Reserve{}, 33, 1024, 0},
		{"the sandbox limit", Capacity{VCPU: 16, MemoryMB: 32768, Sandboxes: 9, MaxSandboxes: 10}, Reserve{}, 1, 1024, 1},
		{"a full server", Capacity{VCPU: 16, MemoryMB: 32768, AllocVCPU: 20}, Reserve{}, 1, 1024, 0},
	}
	for _, tt := range tests {
		if got := tt.cap.Room(tt.vcpu, tt.mem, tt.r); got != tt.want {
			t.Errorf("%s: room for %d, want %d", tt.name, got, tt.want)
		}
	}
}
