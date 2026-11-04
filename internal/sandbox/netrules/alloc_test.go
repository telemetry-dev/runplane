package netrules

import (
	"net/netip"
	"slices"
	"testing"
)

func TestLeaseAddresses(t *testing.T) {
	a, err := NewAllocator(DefaultTapSubnet)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		index       int
		tap         string
		host, guest string
	}{
		{0, "rp-0", "10.214.0.1", "10.214.0.2"},
		{1, "rp-1", "10.214.0.5", "10.214.0.6"},
		{64, "rp-64", "10.214.1.1", "10.214.1.2"},
		{16383, "rp-16383", "10.214.255.253", "10.214.255.254"},
	} {
		l := a.Lease(tt.index)
		if l.Tap != tt.tap || l.Host.String() != tt.host || l.Guest.String() != tt.guest {
			t.Errorf("Lease(%d) = %s %s %s, want %s %s %s", tt.index, l.Tap, l.Host, l.Guest, tt.tap, tt.host, tt.guest)
		}
	}
	if got := a.Lease(1).HostCIDR().String(); got != "10.214.0.5/30" {
		t.Errorf("HostCIDR = %s, want 10.214.0.5/30", got)
	}
}

func TestAcquireGoesRoundAndStopsWhenFull(t *testing.T) {
	a, err := NewAllocator(netip.MustParsePrefix("10.214.0.0/28")) // 4 blocks
	if err != nil {
		t.Fatal(err)
	}
	got := acquireAll(a, 3)
	a.Release(0)
	got = append(got, acquireAll(a, 10)...)
	if want := []int{0, 1, 2, 3, 0}; !slices.Equal(got, want) {
		t.Fatalf("indexes = %v, want %v (index 0 comes back only after 3)", got, want)
	}
}

func TestUseSkipsNamesThatAreNotTaps(t *testing.T) {
	a, err := NewAllocator(netip.MustParsePrefix("10.214.0.0/27")) // 8 blocks
	if err != nil {
		t.Fatal(err)
	}
	a.use([]string{"rp-1", "rp-4", "eth0", "rp-x", "rp-03", "rp--2", "rp-8", "runplane0"})
	if got, want := acquireAll(a, 10), []int{5, 6, 7, 0, 2, 3}; !slices.Equal(got, want) {
		t.Fatalf("free indexes = %v, want %v", got, want)
	}
}

func TestNewAllocatorRejectsBadSubnets(t *testing.T) {
	for _, s := range []string{"fd00::/64", "10.214.0.0/31", "10.214.0.1/16"} {
		if _, err := NewAllocator(netip.MustParsePrefix(s)); err == nil {
			t.Errorf("NewAllocator(%s) returned no error", s)
		}
	}
}

// acquireAll acquires up to n leases and returns their indexes.
func acquireAll(a *Allocator, n int) []int {
	var got []int
	for range n {
		l, err := a.Acquire()
		if err != nil {
			break
		}
		got = append(got, l.Index)
	}
	return got
}
