//go:build linux

package netrules

import (
	"os"

	"github.com/google/nftables"
)

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
		if family == "" || ch.Table.Name == tableName || family == "ip" && ch.Table.Name == "filter" {
			continue
		}
		out = append(out, family+" "+ch.Table.Name+" "+ch.Name)
	}
	return out
}
