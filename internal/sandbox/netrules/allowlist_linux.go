//go:build linux

package netrules

import (
	"context"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// resolvedTTL keeps an address of an allowlist name in the set for this
	// long after the last answer. CDNs rotate addresses, and a sandbox can
	// get an older answer than the host.
	resolvedTTL   = time.Hour
	lookupTimeout = 10 * time.Second
)

// remember stores the lookup results for the names of cfg and drops names
// that cfg no longer has and addresses older than resolvedTTL.
func (m *Manager) remember(cfg Config, found map[string][]netip.Addr, errs map[string]error, now time.Time) {
	_, names, _ := splitAllowlist(cfg.Allowlist)
	maps.DeleteFunc(m.resolved, func(name string, _ map[netip.Addr]time.Time) bool {
		return !slices.Contains(names, name)
	})
	for name, err := range errs {
		if !slices.Contains(names, name) {
			continue
		}
		if !m.failing[name] {
			m.log.Warn("resolve an allowlist name", "name", name, "err", err)
		}
		m.failing[name] = true
	}
	for name, addrs := range found {
		if !slices.Contains(names, name) {
			continue
		}
		delete(m.failing, name)
		if m.resolved[name] == nil {
			m.resolved[name] = map[netip.Addr]time.Time{}
		}
		for _, a := range addrs {
			m.resolved[name][a.Unmap()] = now
		}
	}
	for _, seen := range m.resolved {
		maps.DeleteFunc(seen, func(_ netip.Addr, t time.Time) bool { return now.Sub(t) > resolvedTTL })
	}
}

// allowed returns the allowlist prefixes of cfg and the addresses of its
// names, sorted.
func (m *Manager) allowed(cfg Config) []netip.Prefix {
	prefixes, names, _ := splitAllowlist(cfg.Allowlist)
	for _, name := range names {
		for a := range m.resolved[name] {
			if a.Is4() {
				prefixes = append(prefixes, netip.PrefixFrom(a, 32))
			}
		}
	}
	slices.SortFunc(prefixes, netip.Prefix.Compare)
	return slices.Compact(prefixes)
}

// splitAllowlist sorts allowlist entries into IPv4 prefixes and host names.
// It returns IPv6 entries and wildcards as bad, because sandboxes have IPv4
// only and a wildcard does not resolve.
func splitAllowlist(entries []string) (prefixes []netip.Prefix, names, bad []string) {
	for _, e := range entries {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		p, err := netip.ParsePrefix(e)
		if err != nil {
			if a, aerr := netip.ParseAddr(e); aerr == nil {
				p, err = netip.PrefixFrom(a, a.BitLen()), nil
			}
		}
		switch {
		case err == nil && p.Addr().Is4():
			prefixes = append(prefixes, p.Masked())
		case err == nil || strings.ContainsAny(e, "*/: "):
			bad = append(bad, e)
		default:
			names = append(names, strings.TrimSuffix(e, "."))
		}
	}
	return prefixes, names, bad
}

// resolve resolves IPv4 addresses for names through the sandbox DNS servers,
// so the host sees the same answers as the sandboxes.
func resolve(ctx context.Context, names []string, dns []netip.Addr) (map[string][]netip.Addr, map[string]error) {
	found, errs := map[string][]netip.Addr{}, map[string]error{}
	if len(names) == 0 {
		return found, errs
	}
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	r := resolver(dns)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, name := range names {
		wg.Go(func() {
			addrs, err := r.LookupNetIP(ctx, "ip4", name)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[name] = err
			} else {
				found[name] = addrs
			}
		})
	}
	wg.Wait()
	return found, errs
}

func resolver(dns []netip.Addr) *net.Resolver {
	if len(dns) == 0 {
		return net.DefaultResolver
	}
	var next atomic.Uint32
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			server := dns[int(next.Add(1)-1)%len(dns)]
			var d net.Dialer
			return d.DialContext(ctx, network, netip.AddrPortFrom(server, 53).String())
		},
	}
}
