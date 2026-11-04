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

var errNoSetup = errors.New("netrules: call Setup first")

// Manager owns the nftables tables "inet runplane" and "bridge runplane",
// and the runplane rules in the INPUT, FORWARD, and DOCKER-USER chains of
// the iptables filter table. It changes no other firewall objects. Use one
// Manager for each host.
//
// It keeps the sandbox interfaces in memory and writes the whole table again
// in one atomic batch after each change. Thus the kernel never holds half a
// ruleset, and Setup twice gives the same table.
type Manager struct {
	log *slog.Logger

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

func New(log *slog.Logger) *Manager {
	return &Manager{log: log, resolved: map[string]map[netip.Addr]time.Time{}, failing: map[string]bool{}}
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
// still operate.
func (m *Manager) Setup(ctx context.Context, cfg Config) error {
	if err := cfg.check(); err != nil {
		return err
	}
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
	links := m.links
	if !m.ready {
		var err error
		if links, err = readLinks(); err != nil {
			return err
		}
		if m.ports, err = readPorts(); err != nil {
			return err
		}
	}
	m.remember(cfg, found, errs, time.Now())
	if err := m.write(cfg, links); err != nil {
		return err
	}
	m.ready = true
	if m.stop == nil {
		m.stop, m.done = make(chan struct{}), make(chan struct{})
		go m.loop(m.stop, m.done)
	}
	return syncIptables(ctx, m.iptablesRules(), true)
}

// AddTap lets a VM TAP through: the guest may send from guest only and may
// reach the service port on host. Both addresses must be in the TAP subnet.
func (m *Manager) AddTap(iface string, guest, host netip.Addr) error {
	if !isTap(iface) || !validIfname(iface) {
		return fmt.Errorf("netrules: %q is not a TAP name", iface)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.ready {
		return errNoSetup
	}
	if s := m.cfg.TapSubnet; !s.Contains(guest) || !s.Contains(host) || guest == host {
		return fmt.Errorf("netrules: TAP addresses %s and %s must be two addresses in %s", guest, host, s)
	}
	links := maps.Clone(m.links)
	links[iface] = link{src: netip.PrefixFrom(guest, 32), gateway: host}
	return m.write(m.cfg, links)
}

// RemoveTap removes the rules of a VM TAP. Traffic through a TAP without
// rules drops.
func (m *Manager) RemoveTap(iface string) error {
	if !isTap(iface) {
		return fmt.Errorf("netrules: %q is not a TAP name", iface)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.links[iface]; !ok {
		return nil
	}
	links := maps.Clone(m.links)
	delete(links, iface)
	return m.write(m.cfg, links)
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
	if err := m.write(m.cfg, links); err != nil {
		return err
	}
	return syncIptables(context.Background(), m.iptablesRules(), true)
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

// write writes the tables for cfg and links, and keeps them if that worked.
func (m *Manager) write(cfg Config, links map[string]link) error {
	if err := writeTable(m.policy(cfg, links, m.ports)); err != nil {
		return err
	}
	m.cfg, m.links = cfg, links
	return nil
}

func (m *Manager) policy(cfg Config, links map[string]link, ports map[string]port) policy {
	return policy{cfg: cfg, links: links, ports: ports, allowed: m.allowed(cfg)}
}

func (m *Manager) iptablesRules() map[string][][]string {
	return iptablesRules(m.cfg, m.links)
}

func (m *Manager) loop(stop, done chan struct{}) {
	defer close(done)
	for {
		m.mu.Lock()
		every := m.cfg.Refresh
		m.mu.Unlock()
		select {
		case <-stop:
			return
		case <-time.After(every):
		}
		m.refresh()
	}
}

// refresh resolves the names of the allowlist again and repairs the
// iptables rules.
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
		if err := m.write(m.cfg, m.links); err != nil {
			m.log.Error("update the addresses of the allowlist", "err", err)
		}
	}
	if err := syncIptables(context.Background(), m.iptablesRules(), false); err != nil {
		m.log.Error("repair iptables rules", "err", err)
	}
}
