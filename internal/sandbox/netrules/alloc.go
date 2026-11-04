package netrules

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"strconv"
	"strings"
	"sync"
)

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

// HostCIDR is the host address with the /30 prefix, as CreateTap wants it.
func (l Lease) HostCIDR() netip.Prefix { return netip.PrefixFrom(l.Host, 30) }

func NewAllocator(subnet netip.Prefix) (*Allocator, error) {
	if err := checkTapSubnet(subnet); err != nil {
		return nil, err
	}
	return &Allocator{subnet: subnet, used: map[int]bool{}}, nil
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

// Lease returns the lease of an index, for example for a VM that operated
// before a restart.
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
