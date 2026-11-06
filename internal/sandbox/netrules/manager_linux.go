//go:build linux

package netrules

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/netip"
	"os"
	"slices"
	"sync"
	"time"
)

// refreshEvery is how often the Manager resolves the allowlist names again
// and repairs the iptables rules.
const refreshEvery = time.Minute

var (
	errNoSetup = errors.New("netrules: call Setup first")
	errNoTaps  = errors.New("netrules: this Manager has no TAP range")
)

// Manager owns the nftables tables "inet runplane" and "bridge runplane",
// and the runplane rules in the INPUT, FORWARD, and DOCKER-USER chains of
// the iptables filter table. It changes no other firewall objects. In VM
// mode it also owns the TAP devices and their addresses. Use one Manager
// for each host.
//
// It keeps the sandbox interfaces in memory and writes the whole table again
// in one atomic batch after each change. Thus the kernel never holds half a
// ruleset, and Setup twice gives the same table.
type Manager struct {
	log  *slog.Logger
	taps *allocator // nil without TAPs (container mode)

	mu       sync.Mutex
	cfg      Config
	ready    bool
	links    map[string]link
	ports    map[string]port                     // bridge ports by name
	resolved map[string]map[netip.Addr]time.Time // allowlist name → address → time of the last answer
	failing  map[string]bool                     // allowlist names that did not resolve last time
	stop     chan struct{}
	done     chan struct{}
}

// link is one sandbox interface: a VM TAP or the Docker bridge.
type link struct {
	src     netip.Prefix // the addresses that sandboxes behind it may send from
	gateway netip.Addr   // the host address with the service port
}

// New makes the Manager of this host. tapSubnet is the address range of the
// VM TAPs; it is zero in container mode, which has no TAPs.
func New(log *slog.Logger, tapSubnet netip.Prefix) *Manager {
	m := &Manager{log: log, resolved: map[string]map[netip.Addr]time.Time{}, failing: map[string]bool{}}
	if tapSubnet.IsValid() {
		m.taps = newAllocator(tapSubnet)
	}
	return m
}

// Setup writes the tables and the iptables rules for cfg, enables IPv4
// forwarding, and starts the loop that resolves the names of the allowlist.
// Call it again to change the policy.
//
// Forwarding comes on here and not at boot: Docker sets the FORWARD policy
// to DROP only when it turns on forwarding itself, and runplane starts after
// Docker. Without Docker, the policy stays as the host firewall sets it.
//
// The first Setup keeps the TAPs, bridges, and bridge ports that the tables
// already have, so a restart of runplane does not cut off sandboxes that
// still operate, and the TAP leases of these VMs stay theirs.
func (m *Manager) Setup(ctx context.Context, cfg Config) error {
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("enable ip forwarding: %w", err)
	}
	_, names, bad := splitAllowlist(cfg.Allowlist)
	if len(bad) > 0 {
		m.log.Warn("skip egress allowlist entries; use IPv4 addresses, IPv4 prefixes, or host names", "entries", bad)
	}
	found, errs := resolve(ctx, names, cfg.DNS)

	m.mu.Lock()
	defer m.mu.Unlock()
	links, ports := m.links, m.ports
	if !m.ready {
		var err error
		if links, err = readLinks(); err != nil {
			return err
		}
		if ports, err = readPorts(); err != nil {
			return err
		}
		if m.taps != nil {
			m.taps.use(slices.Collect(maps.Keys(links)))
		}
	}
	m.remember(cfg, found, errs, time.Now())
	if err := m.write(cfg, links, ports); err != nil {
		return err
	}
	m.ready = true
	if m.stop == nil {
		m.stop, m.done = make(chan struct{}), make(chan struct{})
		go m.loop(m.stop, m.done)
	}
	return syncIptables(ctx, iptablesRules(m.cfg, m.tapSubnet(), m.links), true)
}

// AcquireTap reserves a free TAP lease. OpenTap or KeepTap makes it usable,
// and CloseTap ends it.
func (m *Manager) AcquireTap() (Lease, error) {
	if m.taps == nil {
		return Lease{}, errNoTaps
	}
	return m.taps.acquire()
}

// TapLeases is the count of leases that the TAP range has.
func (m *Manager) TapLeases() int {
	if m.taps == nil {
		return 0
	}
	return m.taps.size()
}

// OpenTap makes the persistent TAP device of l, which only a process with
// the uid and the gid can open (the Firecracker process of one VM), and lets
// it through the firewall as KeepTap does.
func (m *Manager) OpenTap(l Lease, uid, gid int) error {
	if err := m.check(l); err != nil {
		return err
	}
	if err := createTap(l.Tap, uid, gid, l.HostCIDR()); err != nil {
		return err
	}
	if err := m.KeepTap(l); err != nil {
		return errors.Join(err, deleteTap(l.Tap))
	}
	return nil
}

// KeepTap marks l as used and lets the interface of l through: the guest may
// send from its address only and may reach the service port on the host
// address. The interface exists already, as after a restart of runplane.
func (m *Manager) KeepTap(l Lease) error {
	if err := m.check(l); err != nil {
		return err
	}
	m.taps.use([]string{l.Tap})
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.ready {
		return errNoSetup
	}
	links := maps.Clone(m.links)
	links[l.Tap] = link{src: netip.PrefixFrom(l.Guest, 32), gateway: l.Host}
	return m.write(m.cfg, links, m.ports)
}

// CloseTap removes the rules and the device of l and frees the lease.
// Traffic through a TAP without rules drops.
func (m *Manager) CloseTap(l Lease) error {
	if err := m.check(l); err != nil {
		return err
	}
	err := errors.Join(m.dropTap(l.Tap), deleteTap(l.Tap))
	m.taps.release(l.Index)
	return err
}

func (m *Manager) dropTap(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.links[name]; !ok {
		return nil
	}
	links := maps.Clone(m.links)
	delete(links, name)
	return m.write(m.cfg, links, m.ports)
}

func (m *Manager) check(l Lease) error {
	switch {
	case m.taps == nil:
		return errNoTaps
	case !m.taps.owns(l):
		return fmt.Errorf("netrules: %+v is not a TAP lease of %s", l, m.taps.subnet)
	}
	return nil
}

func (m *Manager) tapSubnet() netip.Prefix {
	if m.taps == nil {
		return netip.Prefix{}
	}
	return m.taps.subnet
}

// Bridge applies the rules to the runplane Docker bridge: containers may
// send from subnet only and may reach the service port on gateway. It also
// writes the iptables rules for the bridge.
func (m *Manager) Bridge(name string, subnet netip.Prefix, gateway netip.Addr) error {
	if isTap(name) || !validIfname(name) {
		return fmt.Errorf("netrules: bridge name %q is not valid", name)
	}
	if !subnet.Addr().Is4() || subnet != subnet.Masked() || !subnet.Contains(gateway) {
		return fmt.Errorf("netrules: bridge subnet %s must be an IPv4 network with the gateway %s", subnet, gateway)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.ready {
		return errNoSetup
	}
	links := maps.Clone(m.links)
	links[name] = link{src: subnet, gateway: gateway}
	if err := m.write(m.cfg, links, m.ports); err != nil {
		return err
	}
	return syncIptables(context.Background(), iptablesRules(m.cfg, m.tapSubnet(), m.links), true)
}

// Close stops the allowlist loop. The rules stay.
func (m *Manager) Close() error {
	m.mu.Lock()
	stop, done := m.stop, m.done
	m.stop, m.done = nil, nil
	m.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
	return nil
}

// Teardown removes the tables and the runplane iptables rules. IPv4
// forwarding stays on, because other services can need it.
func (m *Manager) Teardown() error {
	m.Close()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ready = false
	m.links, m.ports = nil, nil
	return errors.Join(deleteTable(), syncIptables(context.Background(), nil, true))
}

// write writes the tables, and keeps cfg, links, and ports if that worked.
func (m *Manager) write(cfg Config, links map[string]link, ports map[string]port) error {
	if err := writeTable(policy{cfg: cfg, taps: m.tapSubnet(), links: links, ports: ports, allowed: m.allowed(cfg)}); err != nil {
		return err
	}
	m.cfg, m.links, m.ports = cfg, links, ports
	return nil
}

func (m *Manager) loop(stop, done chan struct{}) {
	defer close(done)
	for {
		select {
		case <-stop:
			return
		case <-time.After(refreshEvery):
			m.refresh()
		}
	}
}

func (m *Manager) refresh() {
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()
	_, names, _ := splitAllowlist(cfg.Allowlist)
	found, errs := resolve(context.Background(), names, cfg.DNS)

	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.ready {
		return
	}
	allowed := m.allowed(m.cfg)
	m.remember(m.cfg, found, errs, time.Now())
	if !slices.Equal(allowed, m.allowed(m.cfg)) {
		if err := m.write(m.cfg, m.links, m.ports); err != nil {
			m.log.Error("update the addresses of the allowlist", "err", err)
		}
	}
	if err := syncIptables(context.Background(), iptablesRules(m.cfg, m.tapSubnet(), m.links), false); err != nil {
		m.log.Error("repair iptables rules", "err", err)
	}
}
