// Package netrules owns the host network parts of sandboxes: the nftables
// tables "inet runplane" and "bridge runplane", the runplane rules in the
// iptables filter table (INPUT, FORWARD, and Docker's DOCKER-USER), and in VM
// mode the TAP devices and their addresses.
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
// until its lease opens.
const TapPrefix = "rp-"

// TunDevice is the device through which the kernel makes TAP devices.
const TunDevice = "/dev/net/tun"

// TableName is the name of the nftables tables "inet runplane" and "bridge
// runplane".
const TableName = "runplane"

// RuleComment marks the iptables rules that runplane owns.
const RuleComment = "runplane"

const dockerUser = "DOCKER-USER"

// RuleChains are the chains of the iptables filter table that can have
// runplane rules.
var RuleChains = []string{"INPUT", "FORWARD", dockerUser}

// DefaultTapSubnet is the VM mode address range. Each VM gets one /30 of it.
var DefaultTapSubnet = netip.MustParsePrefix("10.214.0.0/16")

// leaseBits is the prefix length of the block of one lease.
const leaseBits = 30

// TapLeases is the count of leases in a TAP range.
func TapLeases(subnet netip.Prefix) int { return 1 << (leaseBits - subnet.Bits()) }

// Config is the firewall policy for all sandboxes on the host.
type Config struct {
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

// Firewall is what ReadFirewall reads of the host firewall, for the doctor.
type Firewall struct {
	Drops []string // the nftables chains that block the sandboxes (DropChains)
	// ForwardPolicy is "DROP" when the FORWARD chain of an iptables filter
	// table drops by default, else "ACCEPT", or "" without a FORWARD chain.
	ForwardPolicy string
	DockerUser    bool // an iptables filter table has the DOCKER-USER chain
	// Err tells why the iptables filter tables cannot be read, for example
	// ErrNeedRoot.
	Err error
}

// ErrNeedRoot tells that only root can read the host firewall.
var ErrNeedRoot = errors.New("needs root")

// Lease is the TAP name and the addresses of one VM.
type Lease struct {
	Index int        `json:"index"`
	Tap   string     `json:"tap"`
	Host  netip.Addr `json:"host"` // host side of the TAP, the gateway of the guest
	Guest netip.Addr `json:"guest"`
}

func (l Lease) HostCIDR() netip.Prefix  { return netip.PrefixFrom(l.Host, leaseBits) }
func (l Lease) GuestCIDR() netip.Prefix { return netip.PrefixFrom(l.Guest, leaseBits) }

// allocator gives each VM a /30 of a subnet. Index n owns the block at
// subnet + 4n: the host side of the TAP gets +1 and the guest gets +2.
// The TAP name rp-<n> carries the index.
type allocator struct {
	subnet netip.Prefix

	mu   sync.Mutex
	used map[int]bool
	next int
}

// newAllocator takes an IPv4 subnet of /30 or larger.
func newAllocator(subnet netip.Prefix) *allocator {
	return &allocator{subnet: subnet, used: map[int]bool{}}
}

// acquire returns a free lease. It goes round the range, so a released
// index comes back only after all others were used. Thus connection
// tracking entries of a stopped VM do not meet a new VM.
func (a *allocator) acquire() (Lease, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := a.size()
	for i := range n {
		idx := (a.next + i) % n
		if !a.used[idx] {
			a.used[idx] = true
			a.next = idx + 1
			return a.lease(idx), nil
		}
	}
	return Lease{}, errors.New("netrules: no free TAP address")
}

func (a *allocator) release(index int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.used, index)
}

// lease returns the lease of an index, also of one that is not acquired.
func (a *allocator) lease(index int) Lease {
	base := a.subnet.Addr().As4()
	block := binary.BigEndian.Uint32(base[:]) + uint32(index)*4
	addr := func(k uint32) netip.Addr {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], block+k)
		return netip.AddrFrom4(b)
	}
	return Lease{Index: index, Tap: TapPrefix + strconv.Itoa(index), Host: addr(1), Guest: addr(2)}
}

// owns reports if l is a lease of this range.
func (a *allocator) owns(l Lease) bool {
	return l.Index >= 0 && l.Index < a.size() && l == a.lease(l.Index)
}

// use marks the indexes of these interface names as used. It skips names
// that are not TAP names of this range.
func (a *allocator) use(names []string) {
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

func (a *allocator) size() int { return TapLeases(a.subnet) }

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
