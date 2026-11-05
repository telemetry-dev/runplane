// Package netrules owns the host network parts of sandboxes: the nftables
// tables "inet runplane" and "bridge runplane", the runplane rules in the
// iptables filter table (INPUT, FORWARD, and Docker's DOCKER-USER), VM TAP
// devices, and the allocator for TAP addresses.
package netrules

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"strconv"
	"strings"
	"sync"
)

// TapPrefix starts the name of each VM TAP device. The firewall treats each
// interface with this prefix as a sandbox, so a TAP gets no traffic through
// until AddTap gives it a source address.
const TapPrefix = "rp-"

// DefaultTapSubnet is the VM mode address range. Each VM gets one /30 of it.
var DefaultTapSubnet = netip.MustParsePrefix("10.214.0.0/16")

// Config is the firewall policy for all sandboxes on the host.
type Config struct {
	// TapSubnet is the VM mode range. Leave it zero in container mode.
	TapSubnet netip.Prefix
	// ServicePort is the TCP port of the cache server and the registry
	// mirror on the gateway address of each sandbox.
	ServicePort int
	// DNS servers, all IPv4, stay reachable on port 53, also inside blocked
	// ranges and outside the allowlist. The allowlist names resolve through
	// them too.
	DNS []netip.Addr
	// Allowlist limits egress to these host names, IPv4 addresses, and IPv4
	// prefixes. Empty allows all public addresses. Blocked ranges stay
	// blocked.
	Allowlist []string
}

// Allocator gives each VM a /30 of a subnet. Index n owns the block at
// subnet + 4n: the host side of the TAP gets +1 and the guest gets +2.
// The TAP name rp-<n> carries the index, so Recover can find the used
// indexes again after a restart.
type Allocator struct {
	subnet netip.Prefix

	mu   sync.Mutex
	used map[int]bool
	next int
}

// Lease is the TAP name and the addresses of one VM.
type Lease struct {
	Index int
	Tap   string
	Host  netip.Addr // host side of the TAP, the gateway of the guest
	Guest netip.Addr
}

// HostCIDR is the host address with the /30 prefix.
func (l Lease) HostCIDR() netip.Prefix { return netip.PrefixFrom(l.Host, 30) }

// NewAllocator takes an IPv4 subnet of /30 or larger.
func NewAllocator(subnet netip.Prefix) *Allocator {
	return &Allocator{subnet: subnet, used: map[int]bool{}}
}

// Acquire returns a free lease. It goes round the range, so a released
// index comes back only after all others were used. Thus connection
// tracking entries of a stopped VM do not meet a new VM.
func (a *Allocator) Acquire() (Lease, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := a.size()
	for i := range n {
		idx := (a.next + i) % n
		if !a.used[idx] {
			a.used[idx] = true
			a.next = idx + 1
			return a.Lease(idx), nil
		}
	}
	return Lease{}, errors.New("netrules: no free TAP address")
}

func (a *Allocator) Release(index int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.used, index)
}

// Lease returns the lease of an index, also of one that is not acquired.
func (a *Allocator) Lease(index int) Lease {
	base := a.subnet.Addr().As4()
	block := binary.BigEndian.Uint32(base[:]) + uint32(index)*4
	addr := func(k uint32) netip.Addr {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], block+k)
		return netip.AddrFrom4(b)
	}
	return Lease{Index: index, Tap: TapPrefix + strconv.Itoa(index), Host: addr(1), Guest: addr(2)}
}

// use marks the indexes of these interface names as used. It skips names
// that are not TAP names of this range.
func (a *Allocator) use(names []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, name := range names {
		idx, ok := tapIndex(name)
		if !ok || idx >= a.size() {
			continue
		}
		a.used[idx] = true
		a.next = max(a.next, idx+1)
	}
}

func (a *Allocator) size() int { return 1 << (30 - a.subnet.Bits()) }

func tapIndex(name string) (int, bool) {
	s, ok := strings.CutPrefix(name, TapPrefix)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || strconv.Itoa(n) != s {
		return 0, false
	}
	return n, true
}
