//go:build linux

package netrules

import (
	"bytes"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// port is the sandbox behind one port of a sandbox bridge: the addresses
// that Docker gave its container.
type port struct {
	mac net.HardwareAddr
	ip  netip.Addr
}

// AddPort binds a port of a sandbox bridge, the host end of the veth pair of
// a container, to the MAC and IPv4 address of the container. The bridge then
// drops each frame from the port with other addresses, also ARP, and each
// frame from another port with these addresses. A port without a binding,
// such as the one of a doctor probe, may use the addresses that no port has.
//
// Docker checks no source addresses, and a container root can change them.
// Without the binding, one sandbox could take the address of another and
// pass as that sandbox to the cache server.
func (m *Manager) AddPort(iface string, mac net.HardwareAddr, ip netip.Addr) error {
	if isTap(iface) || !validIfname(iface) {
		return fmt.Errorf("netrules: %q is not a bridge port name", iface)
	}
	if len(mac) != 6 || !ip.Is4() {
		return fmt.Errorf("netrules: port %s needs an Ethernet and an IPv4 address, not %s and %s", iface, mac, ip)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.ready {
		return errNoSetup
	}
	in := false
	for name, l := range m.links {
		in = in || !isTap(name) && l.src.Contains(ip)
	}
	if !in {
		return fmt.Errorf("netrules: port address %s is in no sandbox bridge subnet", ip)
	}
	if p, ok := m.ports[iface]; ok && bytes.Equal(p.mac, mac) && p.ip == ip {
		return nil
	}
	ports := maps.Clone(m.ports)
	ports[iface] = port{mac: mac, ip: ip}
	return m.write(m.cfg, m.links, ports)
}

// RemovePort removes the binding of a bridge port.
func (m *Manager) RemovePort(iface string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.ports[iface]; !ok {
		return nil
	}
	ports := maps.Clone(m.ports)
	delete(ports, iface)
	return m.write(m.cfg, m.links, ports)
}

// HostPeer returns the name of the host end of the veth pair whose other
// end is ifname in the network namespace at nsPath, for example the netns
// file of a Docker container.
func HostPeer(nsPath, ifname string) (string, error) {
	ns, err := netns.GetFromPath(nsPath)
	if err != nil {
		return "", fmt.Errorf("open network namespace %s: %w", nsPath, err)
	}
	defer ns.Close()
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		return "", fmt.Errorf("open network namespace %s: %w", nsPath, err)
	}
	defer h.Close()
	l, err := h.LinkByName(ifname)
	if err != nil {
		return "", fmt.Errorf("find %s in %s: %w", ifname, nsPath, err)
	}
	if l.Type() != "veth" || l.Attrs().ParentIndex == 0 {
		return "", fmt.Errorf("%s in %s is not a veth", ifname, nsPath)
	}
	peer, err := netlink.LinkByIndex(l.Attrs().ParentIndex)
	if err != nil {
		return "", fmt.Errorf("find the host end of %s in %s: %w", ifname, nsPath, err)
	}
	return peer.Attrs().Name, nil
}

// addBridgeTable adds the table "bridge runplane" for p to the batch, in
// the place of the table that exists.
//
//	prerouting: ibrname not @bridges: accept
//	            iifname @bound: jump bound
//	            ether saddr @macs, ip saddr @ips, ARP sender @ips or @macs: drop
//	bound:      IPv4: iifname . ether saddr . ip saddr @ports: accept
//	            ARP: iifname . ether saddr . arp saddr ip @ports and
//	                 iifname . arp saddr ether . arp saddr ip @ports: accept
//	            drop
//
// The hook runs before the bridge learns the source MAC, so a dropped frame
// also cannot move the MAC of a sandbox to another port.
func addBridgeTable(c *nftables.Conn, p policy) error {
	if _, err := dropTable(c, nftables.TableFamilyBridge); err != nil {
		return err
	}
	t := c.AddTable(&nftables.Table{Family: nftables.TableFamilyBridge, Name: tableName})

	bridges := &nftables.Set{Table: t, Name: "bridges", KeyType: nftables.TypeIFName}
	bound := &nftables.Set{Table: t, Name: "bound", KeyType: nftables.TypeIFName}
	macs := &nftables.Set{Table: t, Name: "macs", KeyType: nftables.TypeEtherAddr}
	ips := &nftables.Set{Table: t, Name: "ips", KeyType: nftables.TypeIPAddr}
	ports := &nftables.Set{Table: t, Name: "ports", Concatenation: true,
		KeyType: nftables.MustConcatSetType(nftables.TypeIFName, nftables.TypeEtherAddr, nftables.TypeIPAddr)}
	elems := map[*nftables.Set][]nftables.SetElement{}
	for _, name := range slices.Sorted(maps.Keys(p.links)) {
		if !isTap(name) {
			elems[bridges] = append(elems[bridges], nftables.SetElement{Key: ifname(name)})
		}
	}
	seenMAC, seenIP := map[string]bool{}, map[netip.Addr]bool{}
	for _, name := range slices.Sorted(maps.Keys(p.ports)) {
		pt := p.ports[name]
		elems[bound] = append(elems[bound], nftables.SetElement{Key: ifname(name)})
		elems[ports] = append(elems[ports], nftables.SetElement{Key: portKey(name, pt)})
		if !seenMAC[pt.mac.String()] {
			seenMAC[pt.mac.String()] = true
			elems[macs] = append(elems[macs], nftables.SetElement{Key: pt.mac})
		}
		if !seenIP[pt.ip] {
			seenIP[pt.ip] = true
			elems[ips] = append(elems[ips], nftables.SetElement{Key: pt.ip.AsSlice()})
		}
	}
	for _, set := range []*nftables.Set{bridges, bound, macs, ips, ports} {
		if err := c.AddSet(set, elems[set]); err != nil {
			return fmt.Errorf("add set %s: %w", set.Name, err)
		}
	}

	pre := c.AddChain(&nftables.Chain{Name: "prerouting", Table: t, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookPrerouting, Priority: nftables.ChainPriorityRef(-200)})
	boundChain := c.AddChain(&nftables.Chain{Name: "bound", Table: t})
	add := func(ch *nftables.Chain, e ...expr.Any) { c.AddRule(&nftables.Rule{Table: t, Chain: ch, Exprs: e}) }
	etherType := func(v uint16) []expr.Any {
		return []expr.Any{&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseLLHeader, Offset: 12, Len: 2}, cmp(expr.CmpOpEq, be16(v))}
	}
	load := func(reg uint32, base expr.PayloadBase, offset, n uint32) expr.Any {
		return &expr.Payload{DestRegister: reg, Base: base, Offset: offset, Len: n}
	}
	ll, nh := expr.PayloadBaseLLHeader, expr.PayloadBaseNetworkHeader
	const (
		arpSenderMAC = 8
		arpSenderIP  = 14
		// The key of @ports is the port name in register 1, a MAC in
		// register 2, and an IPv4 address in the 32-bit register after the
		// MAC and its padding.
		ipReg = unix.NFT_REG32_06
	)

	add(pre, meta(expr.MetaKeyBRIIIFNAME, 1), lookup(bridges, true), verdict(expr.VerdictAccept))
	add(pre, meta(expr.MetaKeyIIFNAME, 1), lookup(bound, false), jump(boundChain.Name))
	add(pre, load(1, ll, 6, 6), lookup(macs, false), verdict(expr.VerdictDrop))
	add(pre, append(etherType(unix.ETH_P_IP), load(1, nh, srcOffset, 4), lookup(ips, false), verdict(expr.VerdictDrop))...)
	add(pre, append(etherType(unix.ETH_P_ARP), load(1, nh, arpSenderIP, 4), lookup(ips, false), verdict(expr.VerdictDrop))...)
	add(pre, append(etherType(unix.ETH_P_ARP), load(1, nh, arpSenderMAC, 6), lookup(macs, false), verdict(expr.VerdictDrop))...)

	add(boundChain, append(etherType(unix.ETH_P_IP),
		meta(expr.MetaKeyIIFNAME, 1), load(2, ll, 6, 6), load(ipReg, nh, srcOffset, 4),
		lookup(ports, false), verdict(expr.VerdictAccept))...)
	add(boundChain, append(etherType(unix.ETH_P_ARP),
		meta(expr.MetaKeyIIFNAME, 1), load(2, ll, 6, 6), load(ipReg, nh, arpSenderIP, 4), lookup(ports, false),
		load(2, nh, arpSenderMAC, 6), lookup(ports, false), verdict(expr.VerdictAccept))...)
	add(boundChain, verdict(expr.VerdictDrop))
	return nil
}

// portKey is the key of a port in the set "ports": the name, the MAC with
// two bytes of padding, and the IPv4 address.
func portKey(name string, p port) []byte {
	key := append(ifname(name), p.mac...)
	key = append(key, 0, 0)
	return append(key, p.ip.AsSlice()...)
}

// readPorts reads the port bindings from the bridge table that an earlier
// runplane process wrote. It leaves out the ports that are gone: Docker can
// give their addresses to new containers.
func readPorts() (map[string]port, error) {
	ports := map[string]port{}
	c, err := nftables.New()
	if err != nil {
		return nil, fmt.Errorf("open nftables: %w", err)
	}
	exists, err := tableExists(c, nftables.TableFamilyBridge)
	if err != nil || !exists {
		return ports, err
	}
	t := &nftables.Table{Family: nftables.TableFamilyBridge, Name: tableName}
	elems, err := c.GetSetElements(&nftables.Set{Table: t, Name: "ports"})
	if err != nil {
		return nil, fmt.Errorf("read set ports: %w", err)
	}
	for _, e := range elems {
		if len(e.Key) != unix.IFNAMSIZ+12 {
			continue
		}
		name := string(bytes.TrimRight(e.Key[:unix.IFNAMSIZ], "\x00"))
		if _, err := net.InterfaceByName(name); err != nil {
			continue
		}
		mac := net.HardwareAddr(bytes.Clone(e.Key[unix.IFNAMSIZ : unix.IFNAMSIZ+6]))
		ports[name] = port{mac: mac, ip: netip.AddrFrom4([4]byte(e.Key[unix.IFNAMSIZ+8:]))}
	}
	return ports, nil
}
