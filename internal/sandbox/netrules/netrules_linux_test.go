//go:build linux

package netrules

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// These tests need root and the nft and iptables commands. Each test operates
// in a new network namespace, so it never changes the firewall of the host
// that runs it. Run them in a privileged container:
//
//	docker run --rm --privileged ... go test ./internal/sandbox/netrules/

var testCfg = Config{
	TapSubnet:   DefaultTapSubnet,
	ServicePort: 8486,
	DNS:         []netip.Addr{netip.MustParseAddr("192.168.77.2")},
	Refresh:     time.Hour,
}

func TestSetupIsIdempotentAndLeavesOtherTablesAlone(t *testing.T) {
	isolate(t)
	run(t, "nft", "add table inet other")
	run(t, "nft", "add chain inet other forward { type filter hook forward priority 0; policy accept; }")
	run(t, "nft", "add rule inet other forward ip daddr 203.0.113.9 counter drop")
	other := run(t, "nft", "list table inet other")

	m := newManager(t)
	setup(t, m, testCfg)
	first := run(t, "nft", "list table inet runplane")
	setup(t, m, testCfg)
	if second := run(t, "nft", "list table inet runplane"); second != first {
		t.Fatalf("second Setup changed the table:\n%s\nwant:\n%s", second, first)
	}
	for _, want := range []string{
		`elements = { "rp-*" }`,
		"0.0.0.0/8, 10.0.0.0/8,",
		"224.0.0.0/3", // 224/4 and 240/4 merged, open to the end of the range
	} {
		if !strings.Contains(first, want) {
			t.Errorf("table has no %q:\n%s", want, first)
		}
	}
	if got := run(t, "nft", "list table inet other"); got != other {
		t.Fatalf("Setup changed table inet other:\n%s\nwant:\n%s", got, other)
	}

	if err := m.Teardown(); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("nft", "list", "table", "inet", "runplane").CombinedOutput(); err == nil {
		t.Fatalf("table inet runplane exists after Teardown:\n%s", out)
	}
	if got := run(t, "nft", "list table inet other"); got != other {
		t.Fatalf("Teardown changed table inet other:\n%s\nwant:\n%s", got, other)
	}
}

func TestAddTapAndRemoveTapChangeTheRules(t *testing.T) {
	isolate(t)
	m := newManager(t)
	setup(t, m, testCfg)
	before := run(t, "nft", "list table inet runplane")

	if err := m.AddTap("rp-3", netip.MustParseAddr("10.214.0.14"), netip.MustParseAddr("10.214.0.13")); err != nil {
		t.Fatal(err)
	}
	added := run(t, "nft", "list table inet runplane")
	for _, want := range []string{`"rp-3" . 10.214.0.14`, `"rp-3" . 10.214.0.13`} {
		if !strings.Contains(added, want) {
			t.Errorf("table after AddTap has no %q:\n%s", want, added)
		}
	}

	if err := m.RemoveTap("rp-3"); err != nil {
		t.Fatal(err)
	}
	if got := run(t, "nft", "list table inet runplane"); got != before {
		t.Fatalf("table after RemoveTap:\n%s\nwant the table before AddTap:\n%s", got, before)
	}

	if err := m.AddTap("rp-4", netip.MustParseAddr("10.99.0.2"), netip.MustParseAddr("10.99.0.1")); err == nil {
		t.Error("AddTap accepted a guest outside the TAP subnet, which the masquerade rule does not cover")
	}
	if err := m.AddTap("eth9", netip.MustParseAddr("10.214.0.18"), netip.MustParseAddr("10.214.0.17")); err == nil {
		t.Error("AddTap accepted a name without the TAP prefix, which the rules do not treat as a sandbox")
	}
}

func TestSetupAfterRestartKeepsTapsAndBridges(t *testing.T) {
	isolate(t)
	m := newManager(t)
	setup(t, m, testCfg)
	if err := m.AddTap("rp-0", netip.MustParseAddr("10.214.0.2"), netip.MustParseAddr("10.214.0.1")); err != nil {
		t.Fatal(err)
	}
	if err := m.Bridge("rpbr0", netip.MustParsePrefix("10.215.0.0/16"), netip.MustParseAddr("10.215.0.1")); err != nil {
		t.Fatal(err)
	}
	want := run(t, "nft", "list table inet runplane")
	m.Close()

	restarted := newManager(t)
	setup(t, restarted, testCfg)
	if got := run(t, "nft", "list table inet runplane"); got != want {
		t.Fatalf("table after a restart:\n%s\nwant:\n%s", got, want)
	}
}

func TestDockerUserRulesInsertOnceAndRemoveCleanly(t *testing.T) {
	isolate(t)
	run(t, "iptables-nft", "-N DOCKER-USER")
	run(t, "iptables-nft", "-A DOCKER-USER -s 203.0.113.0/24 -j DROP")
	userRule := "-A DOCKER-USER -s 203.0.113.0/24 -j DROP"

	m := newManager(t)
	setup(t, m, testCfg)
	bridge := func() {
		t.Helper()
		if err := m.Bridge("rpbr0", netip.MustParsePrefix("10.215.0.0/16"), netip.MustParseAddr("10.215.0.1")); err != nil {
			t.Fatal(err)
		}
	}
	bridge()
	first := run(t, "iptables-nft", "-S DOCKER-USER")
	bridge()
	setup(t, m, testCfg)
	if second := run(t, "iptables-nft", "-S DOCKER-USER"); second != first {
		t.Fatalf("DOCKER-USER changed on the second Bridge and Setup:\n%s\nwant:\n%s", second, first)
	}
	for _, want := range []string{
		"-A DOCKER-USER -s 10.214.0.0/16 -m comment --comment runplane -j ACCEPT",
		"-A DOCKER-USER -i rpbr0 -o rpbr0 -m comment --comment runplane -j DROP",
		"-A DOCKER-USER ! -s 10.215.0.0/16 -i rpbr0 -m comment --comment runplane -j DROP",
		"-A DOCKER-USER -d 169.254.0.0/16 -i rpbr0 -m comment --comment runplane -j DROP",
		userRule,
	} {
		if !strings.Contains(first, want+"\n") {
			t.Errorf("DOCKER-USER has no %q:\n%s", want, first)
		}
	}
	if !strings.HasSuffix(first, userRule+"\n") {
		t.Errorf("runplane rules are not before the user rule:\n%s", first)
	}

	// Docker or a firewall reload can flush the chain; the loop repairs it.
	run(t, "iptables-nft", "-F DOCKER-USER")
	m.refresh()
	if got := run(t, "iptables-nft", "-S DOCKER-USER"); strings.Count(got, "runplane") != strings.Count(first, "runplane") {
		t.Fatalf("refresh did not repair DOCKER-USER:\n%s", got)
	}

	if err := m.Teardown(); err != nil {
		t.Fatal(err)
	}
	if got, want := run(t, "iptables-nft", "-S DOCKER-USER"), "-N DOCKER-USER\n"; got != want {
		t.Fatalf("DOCKER-USER after Teardown:\n%s\nwant:\n%s", got, want)
	}
}

func TestDockerUserRulesGoToTheLegacyBackendThatHasTheChain(t *testing.T) {
	isolate(t)
	run(t, "iptables-legacy", "-N DOCKER-USER")

	m := newManager(t)
	setup(t, m, testCfg)
	if got := run(t, "iptables-legacy", "-S DOCKER-USER"); !strings.Contains(got, "-s 10.214.0.0/16 -m comment --comment runplane -j ACCEPT") {
		t.Fatalf("legacy DOCKER-USER has no VM subnet rule:\n%s", got)
	}
	if out, err := exec.Command("iptables-nft", "-S", "DOCKER-USER").CombinedOutput(); err == nil {
		t.Fatalf("Setup made a DOCKER-USER chain in the nf_tables backend:\n%s", out)
	}
}

// A host firewall such as ufw sets the INPUT and FORWARD policies to drop.
// The runplane accepts at the top of these chains let VMs reach the internet
// and sandboxes reach the service port, and Teardown removes them.
func TestHostFirewallWithDropPolicies(t *testing.T) {
	host := isolate(t)
	sb := peer(t, host, "rp-0", "10.214.0.1/30", "10.214.0.2/30")
	wan := peer(t, host, "wan0", "198.51.100.1/24", "198.51.100.2/24")
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	serve(t, host, host, "0.0.0.0:8486")
	serve(t, host, wan, "198.51.100.2:9000")
	run(t, "iptables-nft", "-P INPUT DROP")
	run(t, "iptables-nft", "-P FORWARD DROP")
	run(t, "iptables-nft", "-A INPUT -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT")
	block(t, host, sb, "198.51.100.2:9000")
	block(t, host, sb, "10.214.0.1:8486")

	m := newManager(t)
	setup(t, m, testCfg)
	if err := m.AddTap("rp-0", mustAddr("10.214.0.2"), mustAddr("10.214.0.1")); err != nil {
		t.Fatal(err)
	}
	reach(t, host, sb, "198.51.100.2:9000")
	reach(t, host, sb, "10.214.0.1:8486")

	// A firewall reload drops the rules; the loop puts them back.
	run(t, "iptables-nft", "-D INPUT -i rp-+ -p tcp --dport 8486 -m comment --comment runplane -j ACCEPT")
	m.refresh()
	reach(t, host, sb, "10.214.0.1:8486")

	if err := m.Teardown(); err != nil {
		t.Fatal(err)
	}
	for _, chain := range []string{"INPUT", "FORWARD"} {
		if got := run(t, "iptables-nft", "-S "+chain); strings.Contains(got, "runplane") {
			t.Errorf("%s after Teardown has runplane rules:\n%s", chain, got)
		}
	}
}

// The service port listens on all addresses. Only sandboxes and the host
// itself may connect to it; the agent port and the dashboard stay open.
func TestServicePortOnlyForSandboxesAndTheHost(t *testing.T) {
	host := isolate(t)
	sb := peer(t, host, "rp-0", "10.214.0.1/30", "10.214.0.2/30")
	lan := peer(t, host, "eth1", "198.51.100.1/24", "198.51.100.2/24")
	br := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "rpbr0"}}
	if err := netlink.LinkAdd(br); err != nil {
		t.Fatal(err)
	}
	setUp(t, nil, "rpbr0", "198.18.0.1/24")
	box := bridgePort(t, host, br, "c1", "198.18.0.2/24")
	addRoute(t, lan, "10.214.0.0/16", "198.51.100.1")
	for _, addr := range []string{"0.0.0.0:8486", "0.0.0.0:8485", "0.0.0.0:8484"} {
		serve(t, host, host, addr)
	}
	// Without the rules, the network reaches the service port.
	reach(t, host, lan, "198.51.100.1:8486")
	reach(t, host, lan, "10.214.0.1:8486")

	m := newManager(t)
	setup(t, m, testCfg)
	if err := m.AddTap("rp-0", mustAddr("10.214.0.2"), mustAddr("10.214.0.1")); err != nil {
		t.Fatal(err)
	}
	if err := m.Bridge("rpbr0", netip.MustParsePrefix("198.18.0.0/24"), mustAddr("198.18.0.1")); err != nil {
		t.Fatal(err)
	}
	block(t, host, lan, "198.51.100.1:8486")
	block(t, host, lan, "10.214.0.1:8486") // the gateway address of a VM
	reach(t, host, lan, "198.51.100.1:8485")
	reach(t, host, lan, "198.51.100.1:8484")
	reach(t, host, sb, "10.214.0.1:8486")
	reach(t, host, box, "198.18.0.1:8486")
	reach(t, host, host, "127.0.0.1:8486")
	reach(t, host, host, "198.51.100.1:8486") // the host to its own address goes through lo

	if err := m.Teardown(); err != nil {
		t.Fatal(err)
	}
	reach(t, host, lan, "198.51.100.1:8486")
}

// TestPackets sends real traffic. Network namespaces stand in for two VMs
// (veth devices with TAP names), the internet, and a private network.
func TestPackets(t *testing.T) {
	host := isolate(t)
	sb1 := peer(t, host, "rp-0", "10.214.0.1/30", "10.214.0.2/30")
	sb2 := peer(t, host, "rp-1", "10.214.0.5/30", "10.214.0.6/30")
	wan := peer(t, host, "wan0", "198.51.100.1/24", "198.51.100.2/24")
	lan := peer(t, host, "lan0", "192.168.77.1/24", "192.168.77.2/24")
	addAddr(t, wan, "198.51.100.3/24")
	addRoute(t, wan, "10.214.0.0/16", "198.51.100.1")
	// Reverse path filtering would also drop spoofed packets; turn it off
	// to test the rules alone.
	for f, v := range map[string]string{"ipv4/ip_forward": "1", "ipv4/conf/all/rp_filter": "0", "ipv4/conf/rp-0/rp_filter": "0"} {
		if err := os.WriteFile("/proc/sys/net/"+f, []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	serve(t, host, host, "0.0.0.0:8486")
	serve(t, host, host, "0.0.0.0:2222")
	serve(t, host, sb1, "10.214.0.2:9000")
	serve(t, host, sb2, "10.214.0.6:9000")
	serve(t, host, wan, "198.51.100.2:9000")
	serve(t, host, wan, "198.51.100.3:9000")
	serve(t, host, lan, "192.168.77.2:80")
	serve(t, host, lan, "192.168.77.2:53")
	udp := listenUDP(t, host, wan, "198.51.100.2:9001")

	// Without the rules all paths are open, so each block below comes
	// from the rules.
	reach(t, host, sb1, "10.214.0.6:9000")
	reach(t, host, sb1, "192.168.77.2:80")
	reach(t, host, wan, "10.214.0.2:9000")

	m := newManager(t)
	setup(t, m, testCfg)
	for _, l := range []Lease{{Tap: "rp-0", Host: mustAddr("10.214.0.1"), Guest: mustAddr("10.214.0.2")}, {Tap: "rp-1", Host: mustAddr("10.214.0.5"), Guest: mustAddr("10.214.0.6")}} {
		if err := m.AddTap(l.Tap, l.Guest, l.Host); err != nil {
			t.Fatal(err)
		}
	}

	if got := reach(t, host, sb1, "198.51.100.2:9000"); got != "198.51.100.1" {
		t.Errorf("internet server saw %s, want the masqueraded host address 198.51.100.1", got)
	}
	reach(t, host, sb1, "10.214.0.1:8486")   // service port on its own gateway
	reach(t, host, sb1, "192.168.77.2:53")   // DNS server inside a blocked range
	block(t, host, sb1, "10.214.0.6:9000")   // other sandbox
	block(t, host, sb1, "10.214.0.1:2222")   // other host port
	block(t, host, sb1, "10.214.0.5:8486")   // gateway of the other sandbox
	block(t, host, sb1, "198.51.100.1:8486") // other host address
	block(t, host, sb1, "192.168.77.2:80")   // private network
	block(t, host, wan, "10.214.0.2:9000")   // new connection into a sandbox
	addAddr(t, sb1, "10.214.0.6/32")         // to spoof the address of sb2
	if !udpArrives(t, host, sb1, "", udp) {
		t.Error("datagram from the real source address did not arrive")
	}
	if udpArrives(t, host, sb1, "10.214.0.6", udp) {
		t.Error("datagram from a spoofed source address arrived")
	}

	allow := testCfg
	allow.Allowlist = []string{"198.51.100.3"}
	setup(t, m, allow)
	block(t, host, sb1, "198.51.100.2:9000")
	reach(t, host, sb1, "198.51.100.3:9000")
	reach(t, host, sb1, "192.168.77.2:53") // DNS stays open outside the allowlist

	if err := m.RemoveTap("rp-0"); err != nil {
		t.Fatal(err)
	}
	block(t, host, sb1, "198.51.100.3:9000")
}

// TestBridgePackets does the same for container mode: a Linux bridge with
// two ports stands in for the Docker bridge.
func TestBridgePackets(t *testing.T) {
	host := isolate(t)
	br := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "rpbr0"}}
	if err := netlink.LinkAdd(br); err != nil {
		t.Fatal(err)
	}
	// The subnet is outside the blocked ranges, so the check between
	// containers tests the interface rule and not the blocked set.
	setUp(t, nil, "rpbr0", "198.18.0.1/24")
	c1 := bridgePort(t, host, br, "c1", "198.18.0.2/24")
	c2 := bridgePort(t, host, br, "c2", "198.18.0.3/24")
	wan := peer(t, host, "wan0", "198.51.100.1/24", "198.51.100.2/24")
	addRoute(t, wan, "198.18.0.0/24", "198.51.100.1")
	// Docker loads br_netfilter for a bridge without traffic between
	// containers; then bridged packets pass the forward hook.
	bridged := os.WriteFile("/proc/sys/net/bridge/bridge-nf-call-iptables", []byte("1\n"), 0o644) == nil
	// locktime 0: the host takes each ARP update at once, as it does one
	// second after the last one.
	for f, v := range map[string]string{"ipv4/ip_forward": "1", "ipv4/conf/all/rp_filter": "0", "ipv4/conf/rpbr0/rp_filter": "0", "ipv4/neigh/rpbr0/locktime": "0"} {
		if err := os.WriteFile("/proc/sys/net/"+f, []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	serve(t, host, host, "0.0.0.0:8486")
	serve(t, host, host, "0.0.0.0:2222")
	serve(t, host, c1, "198.18.0.2:9000")
	serve(t, host, c2, "198.18.0.3:9000")
	serve(t, host, wan, "198.51.100.2:9000")
	udp := listenUDP(t, host, wan, "198.51.100.2:9001")
	reach(t, host, c1, "198.18.0.3:9000")
	reach(t, host, wan, "198.18.0.2:9000")
	mac1, mac2 := linkMAC(t, c1, "c1p"), linkMAC(t, c2, "c2p")
	// Without the rules, c1 can take the address of c2: the host then sends
	// the traffic for c2 to c1.
	reach(t, host, c2, "198.18.0.1:8486")
	sendARP(t, host, c1, "c1p", mac1, "198.18.0.3")
	if got := neighMAC(t, "rpbr0", "198.18.0.3"); got != mac1.String() {
		t.Fatalf("host MAC for 198.18.0.3 = %s after an ARP of c1 without rules, want %s", got, mac1)
	}
	run(t, "ip", "neigh flush dev rpbr0")

	cfg := testCfg
	cfg.TapSubnet = netip.Prefix{}
	m := newManager(t)
	setup(t, m, cfg)
	if err := m.Bridge("rpbr0", netip.MustParsePrefix("198.18.0.0/24"), mustAddr("198.18.0.1")); err != nil {
		t.Fatal(err)
	}
	bind := func(m *Manager, ns netns.NsHandle, mac net.HardwareAddr, addr string) {
		t.Helper()
		veth, err := HostPeer(fmt.Sprintf("/proc/self/fd/%d", ns), peerName(t, ns))
		if err != nil {
			t.Fatal(err)
		}
		if err := m.AddPort(veth, mac, mustAddr(addr)); err != nil {
			t.Fatal(err)
		}
	}
	// A port without a binding, such as the one of a doctor probe, passes.
	reach(t, host, c1, "198.18.0.1:8486")
	bind(m, c2, mac2, "198.18.0.3")

	if got := reach(t, host, c1, "198.51.100.2:9000"); got != "198.51.100.1" {
		t.Errorf("internet server saw %s, want the masqueraded host address 198.51.100.1", got)
	}
	reach(t, host, c1, "198.18.0.1:8486")
	block(t, host, c1, "198.18.0.1:2222")
	block(t, host, wan, "198.18.0.2:9000")
	if bridged {
		block(t, host, c1, "198.18.0.3:9000")
	} else {
		t.Log("no br_netfilter: skip the check between containers")
	}
	addAddr(t, c1, "10.99.0.9/32")
	if !udpArrives(t, host, c1, "", udp) {
		t.Error("datagram from the bridge subnet did not arrive")
	}
	if udpArrives(t, host, c1, "10.99.0.9", udp) {
		t.Error("datagram from outside the bridge subnet arrived")
	}

	// c1 takes the address of c2 inside the subnet, first with no binding
	// of its own, then with one.
	addAddr(t, c1, "198.18.0.3/32")
	reach(t, host, c2, "198.18.0.1:8486")
	for _, bound := range []bool{false, true} {
		if bound {
			bind(m, c1, mac1, "198.18.0.2")
		}
		if udpArrives(t, host, c1, "198.18.0.3", udp) {
			t.Errorf("bound %v: datagram from the address of the other container arrived", bound)
		}
		sendARP(t, host, c1, "c1p", mac1, "198.18.0.3")
		sendARP(t, host, c1, "c1p", mac2, "198.18.0.3") // with the MAC of c2 too
		if got := neighMAC(t, "rpbr0", "198.18.0.3"); got != mac2.String() {
			t.Errorf("bound %v: host MAC for 198.18.0.3 = %s after a gratuitous ARP of c1, want the MAC of c2 %s", bound, got, mac2)
		}
	}
	if !udpArrives(t, host, c1, "", udp) {
		t.Error("datagram from the bound address did not arrive")
	}

	// A new Manager, as after a restart, keeps the bindings.
	m.Close()
	restarted := newManager(t)
	setup(t, restarted, cfg)
	if udpArrives(t, host, c1, "198.18.0.3", udp) {
		t.Error("after a restart: datagram from the address of the other container arrived")
	}
	for _, name := range []string{"c1", "c2"} {
		if err := restarted.RemovePort(name); err != nil {
			t.Fatal(err)
		}
	}
	if !udpArrives(t, host, c1, "198.18.0.3", udp) {
		t.Error("datagram from an address that no port has did not arrive after RemovePort")
	}
}

// linkMAC returns the MAC of the link name in ns.
func linkMAC(t *testing.T, ns netns.NsHandle, name string) net.HardwareAddr {
	t.Helper()
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	l, err := h.LinkByName(name)
	if err != nil {
		t.Fatal(err)
	}
	return l.Attrs().HardwareAddr
}

// peerName returns the name of the device in ns other than lo.
func peerName(t *testing.T, ns netns.NsHandle) string {
	t.Helper()
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	links, err := h.LinkList()
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range links {
		if l.Attrs().Name != "lo" {
			return l.Attrs().Name
		}
	}
	t.Fatal("namespace has no device")
	return ""
}

// neighMAC returns the MAC that the host has for ip on dev, or "".
func neighMAC(t *testing.T, dev, ip string) string {
	t.Helper()
	l, err := netlink.LinkByName(dev)
	if err != nil {
		t.Fatal(err)
	}
	list, err := netlink.NeighList(l.Attrs().Index, unix.AF_INET)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range list {
		if n.IP.String() == ip {
			return n.HardwareAddr.String()
		}
	}
	return ""
}

// sendARP sends a gratuitous ARP request from ns on dev, as arping -U
// does: ip is at mac. The frame has the source MAC mac too.
func sendARP(t *testing.T, host, ns netns.NsHandle, dev string, mac net.HardwareAddr, ip string) {
	t.Helper()
	inNS(t, host, ns, func() {
		l, err := netlink.LinkByName(dev)
		if err != nil {
			t.Fatal(err)
		}
		fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(htons(unix.ETH_P_ARP)))
		if err != nil {
			t.Fatal(err)
		}
		defer unix.Close(fd)
		bcast := net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
		a := net.ParseIP(ip).To4()
		frame := append(append(append([]byte{}, bcast...), mac...), 0x08, 0x06)
		frame = append(frame, 0, 1, 0x08, 0, 6, 4, 0, 1) // Ethernet, IPv4, request
		frame = append(append(frame, mac...), a...)
		frame = append(append(frame, make([]byte, 6)...), a...)
		to := &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ARP), Ifindex: l.Attrs().Index, Halen: 6}
		copy(to.Addr[:], bcast)
		if err := unix.Sendto(fd, frame, 0, to); err != nil {
			t.Fatal(err)
		}
	})
	// The host takes the frame in a softirq after the send returns.
	time.Sleep(100 * time.Millisecond)
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

func TestCreateTapLetsTheOwnerOpenIt(t *testing.T) {
	isolate(t)
	if err := CreateTap("rp-7", 4242, 4342, netip.MustParsePrefix("10.214.0.29/30")); err != nil {
		t.Fatal(err)
	}
	if err := openTapAs(t, 4242, 4342, "rp-7"); err != nil {
		t.Errorf("owner cannot open the TAP: %v", err)
	}
	if err := openTapAs(t, 4243, 4342, "rp-7"); err == nil {
		t.Error("a different user can open the TAP")
	}
	// Each VM has its own group too, so the owner uid alone is not enough.
	if err := openTapAs(t, 4242, 4343, "rp-7"); err == nil {
		t.Error("the owner with a different group can open the TAP")
	}
	l, err := netlink.LinkByName("rp-7")
	if err != nil {
		t.Fatal(err)
	}
	addrs, err := netlink.AddrList(l, netlink.FAMILY_V4)
	if err != nil || len(addrs) != 1 || addrs[0].IPNet.String() != "10.214.0.29/30" {
		t.Errorf("addresses = %v (%v), want 10.214.0.29/30", addrs, err)
	}
	if l.Attrs().Flags&net.FlagUp == 0 {
		t.Error("TAP is not up")
	}

	if err := DeleteTap("rp-7"); err != nil {
		t.Fatal(err)
	}
	if _, err := netlink.LinkByName("rp-7"); err == nil {
		t.Fatal("TAP exists after DeleteTap")
	}
	if err := DeleteTap("rp-7"); err != nil {
		t.Fatalf("DeleteTap of a missing TAP: %v", err)
	}
}

func TestRecoverFindsUsedIndexes(t *testing.T) {
	isolate(t)
	a, err := NewAllocator(netip.MustParsePrefix("10.214.0.0/27")) // 8 blocks
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{2, 5} {
		l := a.Lease(i)
		if err := CreateTap(l.Tap, 0, 0, l.HostCIDR()); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Recover(); err != nil {
		t.Fatal(err)
	}
	if got, want := acquireAll(a, 10), []int{6, 7, 0, 1, 3, 4}; !slices.Equal(got, want) {
		t.Fatalf("free indexes after Recover = %v, want %v", got, want)
	}
}

// isolate moves the test into a new network namespace and returns it.
func isolate(t *testing.T) netns.NsHandle {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	for _, cmd := range []string{"nft", "iptables-nft", "iptables-legacy"} {
		if _, err := exec.LookPath(cmd); err != nil {
			t.Skipf("needs %s", cmd)
		}
	}
	// A namespace belongs to a thread. Child processes and netlink
	// sockets of this goroutine inherit it.
	runtime.LockOSThread()
	orig, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	ns, err := netns.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		netns.Set(orig)
		ns.Close()
		orig.Close()
		runtime.UnlockOSThread()
	})
	if err := netlink.LinkSetUp(&netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "lo"}}); err != nil {
		t.Fatal(err)
	}
	return ns
}

func newManager(t *testing.T) *Manager {
	m := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { m.Close() })
	return m
}

func setup(t *testing.T, m *Manager, cfg Config) {
	t.Helper()
	if err := m.Setup(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
}

func run(t *testing.T, name, args string) string {
	t.Helper()
	out, err := exec.Command(name, strings.Fields(args)...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, args, err, out)
	}
	return string(out)
}

// openTapAs opens a TAP the way the Firecracker jailer does: as uid and gid,
// without capabilities. Only the locked test thread changes its
// credentials; the raw syscalls do not touch other threads.
func openTapAs(t *testing.T, uid, gid int, name string) error {
	t.Helper()
	groups, err := unix.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	raw := func(nr, a1, a2, a3 uintptr) {
		if _, _, e := unix.RawSyscall(nr, a1, a2, a3); e != 0 {
			t.Fatalf("syscall %d: %v", nr, e)
		}
	}
	keep := ^uintptr(0)
	raw(unix.SYS_SETGROUPS, 0, 0, 0)
	raw(unix.SYS_SETRESGID, keep, uintptr(gid), keep)
	raw(unix.SYS_SETRESUID, keep, uintptr(uid), keep)
	defer func() {
		raw(unix.SYS_SETRESUID, keep, 0, keep)
		raw(unix.SYS_SETRESGID, keep, 0, keep)
		gids := make([]uint32, len(groups)+1)
		for i, g := range groups {
			gids[i] = uint32(g)
		}
		raw(unix.SYS_SETGROUPS, uintptr(len(groups)), uintptr(unsafe.Pointer(&gids[0])), 0)
	}()

	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return err
	}
	ifr.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI | unix.IFF_VNET_HDR)
	return unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr)
}

// peer makes a namespace behind a veth pair. The host side is hostIf with
// hostCIDR; the namespace side has peerCIDR and a default route to the host.
func peer(t *testing.T, host netns.NsHandle, hostIf, hostCIDR, peerCIDR string) netns.NsHandle {
	t.Helper()
	veth := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: hostIf}, PeerName: hostIf + "p"}
	if err := netlink.LinkAdd(veth); err != nil {
		t.Fatal(err)
	}
	ns, err := netns.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := netns.Set(host); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ns.Close() })
	p, err := netlink.LinkByName(hostIf + "p")
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetNsFd(p, int(ns)); err != nil {
		t.Fatal(err)
	}
	setUp(t, nil, hostIf, hostCIDR)
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	setUp(t, h, "lo", "")
	setUp(t, h, hostIf+"p", peerCIDR)
	// The host side, or the bridge, has the first address of the subnet.
	gw := netip.MustParsePrefix(peerCIDR).Masked().Addr().Next()
	if err := h.RouteAdd(&netlink.Route{Gw: gw.AsSlice()}); err != nil {
		t.Fatal(err)
	}
	return ns
}

// bridgePort makes a namespace behind a veth pair whose host side is a port of br.
func bridgePort(t *testing.T, host netns.NsHandle, br *netlink.Bridge, name, cidr string) netns.NsHandle {
	t.Helper()
	ns := peer(t, host, name, "", cidr)
	l, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetMaster(l, br); err != nil {
		t.Fatal(err)
	}
	return ns
}

func setUp(t *testing.T, h *netlink.Handle, name, cidr string) {
	t.Helper()
	if h == nil {
		h = &netlink.Handle{}
	}
	l, err := h.LinkByName(name)
	if err != nil {
		t.Fatal(err)
	}
	if cidr != "" {
		a, err := netlink.ParseAddr(cidr)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.AddrAdd(l, a); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.LinkSetUp(l); err != nil {
		t.Fatal(err)
	}
}

func addAddr(t *testing.T, ns netns.NsHandle, cidr string) {
	t.Helper()
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	links, err := h.LinkList()
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range links {
		if l.Attrs().Name != "lo" {
			a, _ := netlink.ParseAddr(cidr)
			if err := h.AddrAdd(l, a); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatal("namespace has no device")
}

func addRoute(t *testing.T, ns netns.NsHandle, dst, gw string) {
	t.Helper()
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	_, ipnet, _ := net.ParseCIDR(dst)
	if err := h.RouteAdd(&netlink.Route{Dst: ipnet, Gw: net.ParseIP(gw)}); err != nil {
		t.Fatal(err)
	}
}

// serve listens on addr in ns and writes the client address to each
// connection.
func serve(t *testing.T, host, ns netns.NsHandle, addr string) {
	t.Helper()
	var ln net.Listener
	inNS(t, host, ns, func() {
		var err error
		if ln, err = net.Listen("tcp4", addr); err != nil {
			t.Fatal(err)
		}
	})
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			host, _, _ := net.SplitHostPort(c.RemoteAddr().String())
			io.WriteString(c, host)
			c.Close()
		}
	}()
}

// dial connects from ns to addr and returns the client address that the
// server saw.
func dial(t *testing.T, host, ns netns.NsHandle, addr string) (string, error) {
	t.Helper()
	var c net.Conn
	var err error
	inNS(t, host, ns, func() {
		d := net.Dialer{Timeout: time.Second}
		c, err = d.Dial("tcp4", addr)
	})
	if err != nil {
		return "", err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(time.Second))
	b, err := io.ReadAll(c)
	return string(b), err
}

func reach(t *testing.T, host, ns netns.NsHandle, addr string) string {
	t.Helper()
	got, err := dial(t, host, ns, addr)
	if err != nil || got == "" {
		t.Errorf("connect to %s: %v, want a connection", addr, err)
	}
	return got
}

func block(t *testing.T, host, ns netns.NsHandle, addr string) {
	t.Helper()
	if got, err := dial(t, host, ns, addr); err == nil && got != "" {
		t.Errorf("connect to %s worked, want it blocked", addr)
	}
}

func listenUDP(t *testing.T, host, ns netns.NsHandle, addr string) *net.UDPConn {
	t.Helper()
	var c *net.UDPConn
	inNS(t, host, ns, func() {
		var err error
		if c, err = net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.MustParseAddrPort(addr))); err != nil {
			t.Fatal(err)
		}
	})
	t.Cleanup(func() { c.Close() })
	return c
}

// udpArrives sends a datagram from ns, from the source address src if set,
// to server and reports if it arrived.
func udpArrives(t *testing.T, host, ns netns.NsHandle, src string, server *net.UDPConn) bool {
	t.Helper()
	inNS(t, host, ns, func() {
		var local *net.UDPAddr
		if src != "" {
			local = &net.UDPAddr{IP: net.ParseIP(src)}
		}
		c, err := net.DialUDP("udp4", local, server.LocalAddr().(*net.UDPAddr))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if _, err := c.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	})
	server.SetReadDeadline(time.Now().Add(time.Second))
	_, _, err := server.ReadFrom(make([]byte, 16))
	return err == nil
}

// inNS runs f with the thread in ns. A socket stays in the namespace where
// it was made.
func inNS(t *testing.T, host, ns netns.NsHandle, f func()) {
	t.Helper()
	if err := netns.Set(ns); err != nil {
		t.Fatal(err)
	}
	defer netns.Set(host)
	f()
}

func mustAddr(s string) netip.Addr { return netip.MustParseAddr(s) }
