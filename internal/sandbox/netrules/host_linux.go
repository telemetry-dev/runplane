//go:build linux

package netrules

import (
	"context"
	"os"
	"slices"

	"github.com/google/nftables"
)

// ReadFirewall reads the host firewall where it can block the sandboxes:
// the iptables filter tables that runplane puts its rules in
// (filterBackends), and the nftables drop chains (DropChains).
func ReadFirewall(ctx context.Context) Firewall {
	if os.Geteuid() != 0 {
		return Firewall{Err: ErrNeedRoot}
	}
	f := Firewall{Drops: DropChains()}
	backends, err := filterBackends(ctx)
	if err != nil {
		f.Err = err
		return f
	}
	for _, b := range backends {
		if f.ForwardPolicy != "DROP" && b.forward != "" {
			f.ForwardPolicy = b.forward
		}
		f.DockerUser = f.DockerUser || slices.Contains(b.chains, dockerUser)
	}
	return f
}

// Available reports if the kernel has the nf_tables support that the rules
// need. As root, it loads the module when it is not loaded yet.
func Available() bool {
	if _, err := os.Stat("/sys/module/nf_tables"); err == nil {
		return true
	}
	if os.Geteuid() != 0 {
		return false
	}
	// A netlink request makes the kernel load nf_tables when it is a module.
	c, err := nftables.New()
	if err != nil {
		return false
	}
	_, err = c.ListTables()
	return err == nil
}

// DropChains returns the nftables base chains of the IPv4 input and forward
// hooks with a drop policy, as "family table chain", outside the tables
// where runplane can accept sandbox traffic: its own, and the filter table
// of iptables-nft. In nftables, each base chain must accept, so these chains
// block the sandboxes whatever runplane accepts. It returns nil if it
// cannot read nftables.
func DropChains() []string {
	c, err := nftables.New()
	if err != nil {
		return nil
	}
	chains, err := c.ListChains()
	if err != nil {
		return nil
	}
	var out []string
	for _, ch := range chains {
		if ch.Hooknum == nil || ch.Policy == nil || *ch.Policy != nftables.ChainPolicyDrop {
			continue
		}
		if h := *ch.Hooknum; h != *nftables.ChainHookInput && h != *nftables.ChainHookForward {
			continue
		}
		family := map[nftables.TableFamily]string{nftables.TableFamilyINet: "inet", nftables.TableFamilyIPv4: "ip"}[ch.Table.Family]
		if family == "" || ch.Table.Name == TableName || family == "ip" && ch.Table.Name == "filter" {
			continue
		}
		out = append(out, family+" "+ch.Table.Name+" "+ch.Name)
	}
	return out
}
