//go:build linux

package netrules

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

// blockedNets are the destinations that sandboxes may not reach: private
// networks, carrier-grade NAT (with Alibaba metadata at 100.100.100.200),
// link-local (with cloud metadata at 169.254.169.254), the Azure host
// endpoint (WireServer and HostGAPlugin), loopback, multicast, and reserved
// space.
var blockedNets = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("168.63.129.16/32"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

// policy is everything that the tables hold.
type policy struct {
	cfg     Config
	taps    netip.Prefix // the VM TAP range, zero in container mode
	links   map[string]link
	ports   map[string]port
	allowed []netip.Prefix // used only if cfg.Allowlist is not empty
}

// writeTable replaces the tables "inet runplane" and "bridge runplane" with
// the ones for p in one atomic batch.
//
//	forward:     iifname @ifaces jump egress; oifname @ifaces jump ingress
//	egress:      IPv4 only; source must match @sources; no other sandbox;
//	             DNS servers on port 53; no @blocked; only @allowed (if set)
//	ingress:     replies only
//	input:       iifname @ifaces jump to_host; drop TCP to the service port
//	             from other interfaces than lo
//	to_host:     replies; source must match @sources; the service port on
//	             the gateway (@gateways)
//	postrouting: masquerade @nets to non-sandbox interfaces
//
// See addBridgeTable for the bridge table.
func writeTable(p policy) error {
	c, err := nftables.New()
	if err != nil {
		return fmt.Errorf("open nftables: %w", err)
	}
	if _, err := dropTable(c, nftables.TableFamilyINet); err != nil {
		return err
	}
	if err := addBridgeTable(c, p); err != nil {
		return err
	}
	t := c.AddTable(&nftables.Table{Family: nftables.TableFamilyINet, Name: TableName})

	concat := nftables.MustConcatSetType(nftables.TypeIFName, nftables.TypeIPAddr)
	ifaces := &nftables.Set{Table: t, Name: "ifaces", KeyType: nftables.TypeIFName, Interval: true}
	nets := &nftables.Set{Table: t, Name: "nets", KeyType: nftables.TypeIPAddr, Interval: true}
	blocked := &nftables.Set{Table: t, Name: "blocked", KeyType: nftables.TypeIPAddr, Interval: true}
	allowed := &nftables.Set{Table: t, Name: "allowed", KeyType: nftables.TypeIPAddr, Interval: true}
	dns := &nftables.Set{Table: t, Name: "dns", KeyType: nftables.TypeIPAddr}
	sources := &nftables.Set{Table: t, Name: "sources", KeyType: concat, Interval: true, Concatenation: true}
	gateways := &nftables.Set{Table: t, Name: "gateways", KeyType: concat, Concatenation: true}

	ifaceSpans := []span{wildcard(TapPrefix)}
	var netSpans []span
	if p.taps.IsValid() {
		netSpans = append(netSpans, prefixSpan(p.taps))
	}
	var srcElems, gwElems []nftables.SetElement
	for _, name := range slices.Sorted(maps.Keys(p.links)) {
		l := p.links[name]
		if !isTap(name) {
			ifaceSpans = append(ifaceSpans, span{ifname(name), ifname(name)})
			netSpans = append(netSpans, prefixSpan(l.src))
		}
		s := prefixSpan(l.src)
		srcElems = append(srcElems, nftables.SetElement{Key: concatKey(name, s.first), KeyEnd: concatKey(name, s.last)})
		gwElems = append(gwElems, nftables.SetElement{Key: concatKey(name, l.gateway.AsSlice())})
	}
	var dnsElems []nftables.SetElement
	for _, a := range p.cfg.DNS {
		dnsElems = append(dnsElems, nftables.SetElement{Key: a.AsSlice()})
	}

	for _, s := range []struct {
		set   *nftables.Set
		elems []nftables.SetElement
	}{
		{ifaces, intervalElems(ifaceSpans)},
		{nets, intervalElems(netSpans)},
		{blocked, intervalElems(prefixSpans(blockedNets))},
		{allowed, intervalElems(prefixSpans(p.allowed))},
		{dns, dnsElems},
		{sources, srcElems},
		{gateways, gwElems},
	} {
		if err := c.AddSet(s.set, s.elems); err != nil {
			return fmt.Errorf("add set %s: %w", s.set.Name, err)
		}
	}

	chain := func(name string, typ nftables.ChainType, hook *nftables.ChainHook, prio *nftables.ChainPriority) *nftables.Chain {
		return c.AddChain(&nftables.Chain{Name: name, Table: t, Type: typ, Hooknum: hook, Priority: prio})
	}
	forward := chain("forward", nftables.ChainTypeFilter, nftables.ChainHookForward, nftables.ChainPriorityFilter)
	input := chain("input", nftables.ChainTypeFilter, nftables.ChainHookInput, nftables.ChainPriorityFilter)
	postrouting := chain("postrouting", nftables.ChainTypeNAT, nftables.ChainHookPostrouting, nftables.ChainPriorityNATSource)
	egress := chain("egress", "", nil, nil)
	ingress := chain("ingress", "", nil, nil)
	toHost := chain("to_host", "", nil, nil)

	add := func(ch *nftables.Chain, e ...expr.Any) {
		c.AddRule(&nftables.Rule{Table: t, Chain: ch, Exprs: e})
	}
	// ip4 puts an IPv4 match before rules that read the IPv4 header. It
	// also lets nft print "ip saddr" instead of raw header offsets.
	ip4 := func(e ...expr.Any) []expr.Any {
		return append([]expr.Any{meta(expr.MetaKeyNFPROTO, 1), cmp(expr.CmpOpEq, []byte{unix.NFPROTO_IPV4})}, e...)
	}
	notIP4 := []expr.Any{meta(expr.MetaKeyNFPROTO, 1), cmp(expr.CmpOpNeq, []byte{unix.NFPROTO_IPV4}), verdict(expr.VerdictDrop)}

	add(forward, meta(expr.MetaKeyIIFNAME, 1), lookup(ifaces, false), jump(egress.Name))
	add(forward, meta(expr.MetaKeyOIFNAME, 1), lookup(ifaces, false), jump(ingress.Name))

	add(egress, notIP4...)
	add(egress, ip4(meta(expr.MetaKeyIIFNAME, 1), addr(srcOffset, 2), lookup(sources, true), verdict(expr.VerdictDrop))...)
	add(egress, meta(expr.MetaKeyOIFNAME, 1), lookup(ifaces, false), verdict(expr.VerdictDrop))
	for _, proto := range []byte{unix.IPPROTO_UDP, unix.IPPROTO_TCP} {
		add(egress, ip4(addr(dstOffset, 1), lookup(dns, false),
			meta(expr.MetaKeyL4PROTO, 1), cmp(expr.CmpOpEq, []byte{proto}),
			dport(1), cmp(expr.CmpOpEq, be16(53)), verdict(expr.VerdictAccept))...)
	}
	add(egress, ip4(addr(dstOffset, 1), lookup(blocked, false), reject())...)
	if len(p.cfg.Allowlist) > 0 {
		add(egress, ip4(addr(dstOffset, 1), lookup(allowed, true), reject())...)
	}
	add(egress, verdict(expr.VerdictAccept))

	add(ingress, replies()...)
	add(ingress, verdict(expr.VerdictDrop))

	add(input, meta(expr.MetaKeyIIFNAME, 1), lookup(ifaces, false), jump(toHost.Name))
	// The service port listens on all addresses, but only the sandboxes
	// (to_host above) and the host itself may use it.
	add(input, meta(expr.MetaKeyIIFNAME, 1), cmp(expr.CmpOpNeq, ifname("lo")),
		meta(expr.MetaKeyL4PROTO, 1), cmp(expr.CmpOpEq, []byte{unix.IPPROTO_TCP}),
		dport(1), cmp(expr.CmpOpEq, be16(uint16(p.cfg.ServicePort))), verdict(expr.VerdictDrop))

	add(toHost, replies()...)
	add(toHost, notIP4...)
	add(toHost, ip4(meta(expr.MetaKeyIIFNAME, 1), addr(srcOffset, 2), lookup(sources, true), verdict(expr.VerdictDrop))...)
	add(toHost, ip4(meta(expr.MetaKeyIIFNAME, 1), addr(dstOffset, 2), lookup(gateways, false),
		meta(expr.MetaKeyL4PROTO, 1), cmp(expr.CmpOpEq, []byte{unix.IPPROTO_TCP}),
		dport(1), cmp(expr.CmpOpEq, be16(uint16(p.cfg.ServicePort))), verdict(expr.VerdictAccept))...)
	add(toHost, reject())

	add(postrouting, ip4(addr(srcOffset, 1), lookup(nets, false),
		meta(expr.MetaKeyOIFNAME, 1), lookup(ifaces, true), &expr.Masq{})...)

	if err := c.Flush(); err != nil {
		return fmt.Errorf("write nftables table: %w", err)
	}
	return nil
}

// deleteTable deletes the tables "inet runplane" and "bridge runplane".
func deleteTable() error {
	c, err := nftables.New()
	if err != nil {
		return fmt.Errorf("open nftables: %w", err)
	}
	inet, err := dropTable(c, nftables.TableFamilyINet)
	if err != nil {
		return err
	}
	bridge, err := dropTable(c, nftables.TableFamilyBridge)
	if err != nil {
		return err
	}
	if !inet && !bridge {
		return nil
	}
	if err := c.Flush(); err != nil {
		return fmt.Errorf("delete nftables table: %w", err)
	}
	return nil
}

func tableExists(c *nftables.Conn, family nftables.TableFamily) (bool, error) {
	tables, err := c.ListTablesOfFamily(family)
	if err != nil {
		return false, fmt.Errorf("list nftables tables: %w", err)
	}
	return slices.ContainsFunc(tables, func(t *nftables.Table) bool { return t.Name == TableName }), nil
}

// dropTable adds the deletion of the runplane table of family to the batch
// if the table exists, and reports if it does.
func dropTable(c *nftables.Conn, family nftables.TableFamily) (bool, error) {
	exists, err := tableExists(c, family)
	if exists {
		c.DelTable(&nftables.Table{Family: family, Name: TableName})
	}
	return exists, err
}

// readLinks reads the sandbox interfaces from the table that an earlier
// runplane process wrote.
func readLinks() (map[string]link, error) {
	links := map[string]link{}
	c, err := nftables.New()
	if err != nil {
		return nil, fmt.Errorf("open nftables: %w", err)
	}
	exists, err := tableExists(c, nftables.TableFamilyINet)
	if err != nil || !exists {
		return links, err
	}
	t := &nftables.Table{Family: nftables.TableFamilyINet, Name: TableName}
	sources, err := c.GetSetElements(&nftables.Set{Table: t, Name: "sources"})
	if err != nil {
		return nil, fmt.Errorf("read set sources: %w", err)
	}
	gateways, err := c.GetSetElements(&nftables.Set{Table: t, Name: "gateways"})
	if err != nil {
		return nil, fmt.Errorf("read set gateways: %w", err)
	}
	src := map[string]netip.Prefix{}
	for _, e := range sources {
		name, first, ok := splitConcatKey(e.Key)
		if !ok {
			continue
		}
		last := first
		if _, end, ok := splitConcatKey(e.KeyEnd); ok {
			last = end
		}
		if p, ok := rangePrefix(first, last); ok {
			src[name] = p
		}
	}
	for _, e := range gateways {
		name, gw, ok := splitConcatKey(e.Key)
		if p, found := src[name]; ok && found {
			links[name] = link{src: p, gateway: gw}
		}
	}
	return links, nil
}

const (
	srcOffset = 12 // of the source address in the IPv4 header
	dstOffset = 16
)

func meta(key expr.MetaKey, reg uint32) expr.Any { return &expr.Meta{Key: key, Register: reg} }

func addr(offset, reg uint32) expr.Any {
	return &expr.Payload{DestRegister: reg, Base: expr.PayloadBaseNetworkHeader, Offset: offset, Len: 4}
}

// dport loads the destination port of TCP or UDP.
func dport(reg uint32) expr.Any {
	return &expr.Payload{DestRegister: reg, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2}
}

func cmp(op expr.CmpOp, data []byte) expr.Any { return &expr.Cmp{Op: op, Register: 1, Data: data} }

func lookup(s *nftables.Set, invert bool) expr.Any {
	return &expr.Lookup{SourceRegister: 1, SetName: s.Name, SetID: s.ID, Invert: invert}
}

func verdict(k expr.VerdictKind) expr.Any { return &expr.Verdict{Kind: k} }

func jump(chain string) expr.Any { return &expr.Verdict{Kind: expr.VerdictJump, Chain: chain} }

// reject answers with "administratively prohibited", so a job fails fast
// instead of waiting for a timeout.
func reject() expr.Any {
	return &expr.Reject{Type: unix.NFT_REJECT_ICMPX_UNREACH, Code: unix.NFT_REJECT_ICMPX_ADMIN_PROHIBITED}
}

// replies accepts packets of established connections and related ICMP.
func replies() []expr.Any {
	mask := binary.NativeEndian.AppendUint32(nil, expr.CtStateBitESTABLISHED|expr.CtStateBitRELATED)
	return []expr.Any{
		&expr.Ct{Register: 1, Key: expr.CtKeySTATE},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: mask, Xor: make([]byte, 4)},
		cmp(expr.CmpOpNeq, make([]byte, 4)),
		verdict(expr.VerdictAccept),
	}
}

func be16(v uint16) []byte { return binary.BigEndian.AppendUint16(nil, v) }

func ifname(name string) []byte {
	b := make([]byte, unix.IFNAMSIZ)
	copy(b, name)
	return b
}

// wildcard matches all interface names that start with prefix, as "rp-*"
// does in nft.
func wildcard(prefix string) span {
	last := bytes.Repeat([]byte{0xff}, unix.IFNAMSIZ)
	copy(last, prefix)
	return span{ifname(prefix), last}
}

func concatKey(name string, ip []byte) []byte { return append(ifname(name), ip...) }

func splitConcatKey(key []byte) (string, netip.Addr, bool) {
	if len(key) != unix.IFNAMSIZ+4 {
		return "", netip.Addr{}, false
	}
	name := strings.TrimRight(string(key[:unix.IFNAMSIZ]), "\x00")
	return name, netip.AddrFrom4([4]byte(key[unix.IFNAMSIZ:])), true
}

// span is an inclusive range of big-endian keys.
type span struct{ first, last []byte }

func prefixSpan(p netip.Prefix) span {
	first := p.Masked().Addr().As4()
	last := first
	host := ^uint32(0) >> p.Bits()
	binary.BigEndian.PutUint32(last[:], binary.BigEndian.Uint32(first[:])|host)
	return span{first[:], last[:]}
}

func prefixSpans(ps []netip.Prefix) []span {
	spans := make([]span, len(ps))
	for i, p := range ps {
		spans[i] = prefixSpan(p)
	}
	return spans
}

// rangePrefix returns the prefix that covers exactly first to last.
func rangePrefix(first, last netip.Addr) (netip.Prefix, bool) {
	for bits := 32; bits >= 0; bits-- {
		p := netip.PrefixFrom(first, bits)
		if p.Masked().Addr() != first {
			break
		}
		s := prefixSpan(p)
		if netip.AddrFrom4([4]byte(s.last)) == last {
			return p, true
		}
	}
	return netip.Prefix{}, false
}

// intervalElems encodes spans the way nft does for an interval set: a start
// element and an end element one past the last key. It merges spans that
// overlap or touch, because the kernel refuses overlaps.
func intervalElems(spans []span) []nftables.SetElement {
	if len(spans) == 0 {
		return nil
	}
	slices.SortFunc(spans, func(a, b span) int { return bytes.Compare(a.first, b.first) })
	merged := []span{spans[0]}
	for _, s := range spans[1:] {
		prev := &merged[len(merged)-1]
		if next := increment(prev.last); next == nil || bytes.Compare(s.first, next) <= 0 {
			if bytes.Compare(s.last, prev.last) > 0 {
				prev.last = s.last
			}
			continue
		}
		merged = append(merged, s)
	}
	var elems []nftables.SetElement
	// nft also starts with an end element at zero, so that older kernels
	// see the keys before the first span as outside the set.
	if !allZero(merged[0].first) {
		elems = append(elems, nftables.SetElement{Key: make([]byte, len(merged[0].first)), IntervalEnd: true})
	}
	for _, s := range merged {
		elems = append(elems, nftables.SetElement{Key: s.first})
		if end := increment(s.last); end != nil {
			elems = append(elems, nftables.SetElement{Key: end, IntervalEnd: true})
		}
	}
	return elems
}

// increment returns b+1 as a big-endian number, or nil if b is all ones.
func increment(b []byte) []byte {
	out := slices.Clone(b)
	for i := len(out) - 1; i >= 0; i-- {
		out[i]++
		if out[i] != 0 {
			return out
		}
	}
	return nil
}

func allZero(b []byte) bool { return !slices.ContainsFunc(b, func(v byte) bool { return v != 0 }) }

func isTap(name string) bool { return strings.HasPrefix(name, TapPrefix) }

func validIfname(name string) bool {
	return name != "" && len(name) < unix.IFNAMSIZ && !strings.ContainsAny(name, " \t\n/")
}
