package api

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"
)

// DomainDNS tells if the DNS record of the domain setting points to this
// server. Let's Encrypt connects to that address for the certificate.
type DomainDNS struct {
	Domain          string   `json:"domain" doc:"The domain setting. Empty: no domain."`
	State           string   `json:"state" enum:"none,ok,missing,other,unknown,error" doc:"none: no domain is set. ok: each record is an address of this server. missing: the domain has no A or AAAA record. other: a record is not an address of this server. unknown: runplane knows no public address of this server to compare. error: the DNS lookup failed."`
	Records         []string `json:"records" doc:"The addresses of the A and AAAA records of the domain."`
	ServerAddresses []string `json:"server_addresses" doc:"Public IP addresses of this server for the DNS record, IPv4 first. Empty when runplane cannot find one, for example behind NAT."`
	Error           string   `json:"error" doc:"Why the DNS lookup failed."`
}

// States of DomainDNS.
const (
	dnsNone    = "none"
	dnsOK      = "ok"
	dnsMissing = "missing"
	dnsOther   = "other"
	dnsUnknown = "unknown"
	dnsError   = "error"
)

func (h *Handler) getDomainDNS(ctx context.Context, _ *struct{}) (*struct{ Body DomainDNS }, error) {
	st, err := h.d.Store.GetSettings(ctx)
	if err != nil {
		return nil, h.internal(err)
	}
	server := serverAddrs(clientOf(ctx).host)
	out := DomainDNS{Domain: st.Domain, State: dnsNone, Records: []string{}, ServerAddresses: addrStrings(server)}
	if st.Domain == "" {
		return &struct{ Body DomainDNS }{out}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	records, err := lookupDomain(ctx, st.Domain)
	if dnsErr, ok := errors.AsType[*net.DNSError](err); err != nil && (!ok || !dnsErr.IsNotFound) {
		out.State, out.Error = dnsError, err.Error()
		return &struct{ Body DomainDNS }{out}, nil
	}
	out.Records, out.State = addrStrings(records), dnsState(records, server)
	return &struct{ Body DomainDNS }{out}, nil
}

func dnsState(records, server []netip.Addr) string {
	switch {
	case len(records) == 0:
		return dnsMissing
	case len(server) == 0:
		return dnsUnknown
	}
	for _, a := range records {
		if !slices.Contains(server, a.Unmap()) {
			return dnsOther
		}
	}
	return dnsOK
}

// lookupDomain returns the A and AAAA addresses of domain. It asks a
// nameserver of the zone first, because a resolver keeps a "no such host"
// answer for the negative TTL of the zone, 30 minutes at Cloudflare. A record
// that the admin made a minute ago then shows at once. A CNAME to a
// different zone needs the resolver.
func lookupDomain(ctx context.Context, domain string) ([]netip.Addr, error) {
	if ns := zoneNameserver(ctx, domain); ns != "" {
		at := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, net.JoinHostPort(ns, "53"))
		}}
		nsCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		addrs, err := at.LookupNetIP(nsCtx, "ip", domain)
		cancel()
		if err == nil {
			return addrs, nil
		}
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip", domain)
}

// zoneNameserver returns a nameserver of the zone of name: from the NS
// records of name or of its nearest parent that has them. It returns "" when
// it finds none.
func zoneNameserver(ctx context.Context, name string) string {
	for ; strings.Contains(name, "."); _, name, _ = strings.Cut(name, ".") {
		if ns, err := net.DefaultResolver.LookupNS(ctx, name); err == nil && len(ns) > 0 {
			return ns[0].Host
		}
	}
	return ""
}

// serverAddrs returns the public addresses of this server: the addresses of
// its network interfaces, and the address in the Host header of the request.
// Behind NAT, only the request can have the public address.
func serverAddrs(host string) []netip.Addr {
	var addrs []netip.Addr
	if ifAddrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range ifAddrs {
			if p, err := netip.ParsePrefix(a.String()); err == nil {
				addrs = append(addrs, p.Addr())
			}
		}
	}
	if ap, err := netip.ParseAddrPort(host); err == nil {
		addrs = append(addrs, ap.Addr())
	} else if a, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		addrs = append(addrs, a)
	}
	return publicAddrs(addrs)
}

// publicAddrs returns the addresses that a computer on the internet can
// connect to, sorted with IPv4 first and without duplicates.
func publicAddrs(addrs []netip.Addr) []netip.Addr {
	var out []netip.Addr
	for _, a := range addrs {
		a = a.Unmap()
		if a.IsGlobalUnicast() && !a.IsPrivate() && !sharedSpace.Contains(a) && !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	slices.SortFunc(out, netip.Addr.Compare)
	return out
}

// sharedSpace is the range of carrier-grade NAT (RFC 6598). Tailscale also
// uses it.
var sharedSpace = netip.MustParsePrefix("100.64.0.0/10")

func addrStrings(addrs []netip.Addr) []string {
	out := make([]string, len(addrs))
	for i, a := range addrs {
		out[i] = a.Unmap().String()
	}
	return out
}
