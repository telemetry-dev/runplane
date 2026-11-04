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

	"github.com/runplane/runplane/internal/sandbox/netrules"
)

// probe fetches url from a network namespace that has the address of a VM
// on lease l. A veth with the TAP name stands in for the TAP, so the VM
// firewall rules apply to it.
func (h systemHost) probe(ctx context.Context, l netrules.Lease, dns netip.Addr, url string) error {
	ns, err := newNetns()
	if err != nil {
		return fmt.Errorf("make the probe namespace: %w", err)
	}
	defer ns.Close()
	veth := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: l.Tap}, PeerName: "eth0", PeerNamespace: netlink.NsFd(ns)}
	if err := netlink.LinkAdd(veth); err != nil {
		return fmt.Errorf("make the probe interface %s: %w", l.Tap, err)
	}
	defer netrules.DeleteTap(l.Tap)
	outside, err := netlink.NewHandle()
	if err != nil {
		return err
	}
	defer outside.Close()
	inside, err := netlink.NewHandleAt(ns)
	if err != nil {
		return err
	}
	defer inside.Close()
	if err := setUp(outside, l.Tap, l.HostCIDR(), netip.Addr{}); err != nil {
		return err
	}
	if err := setUp(inside, "lo", netip.Prefix{}, netip.Addr{}); err != nil {
		return err
	}
	if err := setUp(inside, "eth0", netip.PrefixFrom(l.Guest, 30), l.Host); err != nil {
		return err
	}
	if err := h.net.AddTap(l.Tap, l.Guest, l.Host); err != nil {
		return err
	}
	defer h.net.RemoveTap(l.Tap)
	return fetchIn(ctx, ns, dns, url)
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

// newNetns makes a network namespace. The thread that makes it ends with
// its goroutine, so no other Go code runs in the namespace by mistake.
func newNetns() (netns.NsHandle, error) {
	type result struct {
		ns  netns.NsHandle
		err error
	}
	ch := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		ns, err := netns.New()
		ch <- result{ns, err}
	}()
	r := <-ch
	return r.ns, r.err
}

// fetchIn fetches url from the namespace ns, with dns as the only DNS
// server, and expects a status below 400.
func fetchIn(ctx context.Context, ns netns.NsHandle, dns netip.Addr, url string) error {
	dial, stop, err := dialIn(ns)
	if err != nil {
		return err
	}
	defer stop()
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

// dialIn returns a dial function whose sockets are in the namespace ns. A
// socket belongs to the namespace of the thread that makes it, so one
// goroutine on a thread in ns makes all of them. The Go resolver and HTTP
// client call dial from other goroutines. stop ends that goroutine.
func dialIn(ns netns.NsHandle) (dial func(ctx context.Context, network, addr string) (net.Conn, error), stop func(), err error) {
	type result struct {
		conn net.Conn
		err  error
	}
	type request struct {
		ctx           context.Context
		network, addr string
		done          chan result
	}
	requests := make(chan request)
	quit := make(chan struct{})
	ready := make(chan error, 1)
	go func() {
		// The thread stays in ns and ends with the goroutine.
		runtime.LockOSThread()
		if err := netns.Set(ns); err != nil {
			ready <- err
			return
		}
		ready <- nil
		var d net.Dialer
		for {
			select {
			case r := <-requests:
				c, err := d.DialContext(r.ctx, r.network, r.addr)
				r.done <- result{c, err}
			case <-quit:
				return
			}
		}
	}()
	if err := <-ready; err != nil {
		return nil, nil, fmt.Errorf("enter the probe namespace: %w", err)
	}
	dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if _, err := netip.ParseAddrPort(addr); err != nil {
			// A host name would make the dialer look it up outside ns.
			return nil, fmt.Errorf("probe: %s is not an IP address and port", addr)
		}
		r := request{ctx, network, addr, make(chan result, 1)}
		select {
		case requests <- r:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-quit:
			return nil, net.ErrClosed
		}
		res := <-r.done
		return res.conn, res.err
	}
	return dial, func() { close(quit) }, nil
}
