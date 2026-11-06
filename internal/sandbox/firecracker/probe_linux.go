//go:build linux

package firecracker

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"runtime"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/netrules"
)

// probe fetches sandbox.ProbeURL from a network namespace that has the
// address of a VM on lease l. A veth with the TAP name stands in for the
// TAP, so the VM firewall rules apply to it. The caller closes the lease,
// which also removes the veth.
func (h systemHost) probe(ctx context.Context, l netrules.Lease, dns netip.Addr) error {
	ns, err := onOwnThread(netns.New)
	if err != nil {
		return fmt.Errorf("make the probe namespace: %w", err)
	}
	defer ns.Close()
	veth := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: l.Tap}, PeerName: "eth0", PeerNamespace: netlink.NsFd(ns)}
	if err := netlink.LinkAdd(veth); err != nil {
		return fmt.Errorf("make the probe interface %s: %w", l.Tap, err)
	}
	inside, err := netlink.NewHandleAt(ns)
	if err != nil {
		return err
	}
	defer inside.Close()
	if err := setUp(&netlink.Handle{}, l.Tap, l.HostCIDR(), netip.Addr{}); err != nil {
		return err
	}
	if err := setUp(inside, "lo", netip.Prefix{}, netip.Addr{}); err != nil {
		return err
	}
	if err := setUp(inside, "eth0", l.GuestCIDR(), l.Host); err != nil {
		return err
	}
	if err := h.net.KeepTap(l); err != nil {
		return err
	}
	return fetchIn(ctx, ns, dns, sandbox.ProbeURL)
}

// setUp gives the link name the address addr, sets it up, and adds a
// default route through gateway. Zero values skip the address or route.
func setUp(h *netlink.Handle, name string, addr netip.Prefix, gateway netip.Addr) error {
	l, err := h.LinkByName(name)
	if err != nil {
		return err
	}
	if addr.IsValid() {
		ipnet := &net.IPNet{IP: addr.Addr().AsSlice(), Mask: net.CIDRMask(addr.Bits(), 32)}
		if err := h.AddrAdd(l, &netlink.Addr{IPNet: ipnet}); err != nil {
			return fmt.Errorf("address %s on %s: %w", addr, name, err)
		}
	}
	if err := h.LinkSetUp(l); err != nil {
		return fmt.Errorf("set %s up: %w", name, err)
	}
	if gateway.IsValid() {
		if err := h.RouteAdd(&netlink.Route{LinkIndex: l.Attrs().Index, Gw: gateway.AsSlice()}); err != nil {
			return fmt.Errorf("route through %s: %w", gateway, err)
		}
	}
	return nil
}

// fetchIn fetches url from the namespace ns, with dns as the only DNS
// server, and expects a status below 400. A socket belongs to the namespace
// of the thread that makes it, so each dial moves a thread of its own to
// ns. The dial addresses are IP addresses: a host name would make the
// dialer look it up outside ns.
func fetchIn(ctx context.Context, ns netns.NsHandle, dns netip.Addr, url string) error {
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return onOwnThread(func() (net.Conn, error) {
			if err := netns.Set(ns); err != nil {
				return nil, fmt.Errorf("enter the probe namespace: %w", err)
			}
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		})
	}
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dial(ctx, network, netip.AddrPortFrom(dns, 53).String())
	}}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := resolver.LookupNetIP(ctx, "ip4", host)
		if err != nil {
			return nil, fmt.Errorf("look up %s through %s: %w", host, dns, err)
		}
		return dial(ctx, "tcp4", net.JoinHostPort(ips[0].String(), port))
	}}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return nil
}

// onOwnThread runs fn on a locked thread that ends with it, so that no
// other Go code runs in a namespace that fn enters or makes.
func onOwnThread[T any](fn func() (T, error)) (T, error) {
	type result struct {
		v   T
		err error
	}
	ch := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		v, err := fn()
		ch <- result{v, err}
	}()
	r := <-ch
	return r.v, r.err
}
