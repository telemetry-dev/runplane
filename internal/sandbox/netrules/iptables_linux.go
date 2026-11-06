//go:build linux

package netrules

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/nftables"
)

// iptablesRules returns the runplane rules of each chain of the iptables
// filter table, in order.
//
// A host firewall such as ufw drops in INPUT and FORWARD by default, and in
// nftables an accept in one base chain does not stop a drop in another. So
// INPUT accepts the service port from each sandbox interface, and in VM
// mode FORWARD accepts the traffic from the TAPs and the replies to them.
// These rules only accept; the table "inet runplane" still applies its
// drops.
//
// Docker reads DOCKER-USER first for forwarded traffic. In VM mode its rules
// accept the TAP subnet, because Docker sets the FORWARD policy to drop; the
// nftables table filters that traffic. In container mode they repeat the
// fixed drops for each bridge. The allowlist stays in nftables only.
func iptablesRules(cfg Config, taps netip.Prefix, links map[string]link) map[string][][]string {
	port := strconv.Itoa(cfg.ServicePort)
	var input, forward, docker [][]string
	if s := taps.String(); taps.IsValid() {
		input = append(input, []string{"-i", TapPrefix + "+", "-p", "tcp", "--dport", port, "-j", "ACCEPT"})
		forward = append(forward,
			[]string{"-i", TapPrefix + "+", "-j", "ACCEPT"},
			[]string{"-o", TapPrefix + "+", "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"})
		docker = append(docker,
			[]string{"-s", s, "-j", "ACCEPT"},
			[]string{"-d", s, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"})
	}
	for _, name := range slices.Sorted(maps.Keys(links)) {
		if isTap(name) {
			continue
		}
		input = append(input, []string{"-i", name, "-p", "tcp", "--dport", port, "-j", "ACCEPT"})
		docker = append(docker,
			[]string{"-i", name, "-o", name, "-j", "DROP"},
			[]string{"-i", name, "!", "-s", links[name].src.String(), "-j", "DROP"})
		for _, dns := range cfg.DNS {
			for _, proto := range []string{"udp", "tcp"} {
				docker = append(docker, []string{"-i", name, "-d", dns.String(), "-p", proto, "--dport", "53", "-j", "RETURN"})
			}
		}
		for _, p := range blockedNets {
			docker = append(docker, []string{"-i", name, "-d", p.String(), "-j", "DROP"})
		}
	}
	rules := map[string][][]string{"INPUT": input, "FORWARD": forward, dockerUser: docker}
	for _, chain := range rules {
		for i := range chain {
			chain[i] = append(chain[i], "-m", "comment", "--comment", RuleComment)
		}
	}
	return rules
}

// syncIptables writes the runplane rules of each chain in want to each
// iptables backend whose filter table has the chain. Without force, it
// writes a backend only if its count of runplane rules differs, for example
// after Docker or a firewall reload removed them. A nil want removes them.
func syncIptables(ctx context.Context, want map[string][][]string, force bool) error {
	backends, err := filterBackends(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, b := range backends {
		if !force && b.inSync(ctx, want) {
			continue
		}
		errs = append(errs, b.sync(ctx, want))
	}
	return errors.Join(errs...)
}

// iptables is one iptables backend with a filter table, and its chains.
type iptables struct {
	cmd, restore string
	chains       []string
}

// filterBackends returns the iptables backends that have a filter table.
// Docker and host firewalls use the backend of the iptables command on the
// host, which can be nf_tables or legacy, so runplane looks in both. It
// looks without side effects: a listing with iptables-legacy can load the
// legacy tables, and an iptables-nft command can make the filter table.
func filterBackends(ctx context.Context) ([]iptables, error) {
	var found []iptables
	c, err := nftables.New()
	if err != nil {
		return nil, fmt.Errorf("open nftables: %w", err)
	}
	chains, err := c.ListChainsOfTableFamily(nftables.TableFamilyIPv4)
	if err != nil {
		return nil, fmt.Errorf("list nftables chains: %w", err)
	}
	var names []string
	for _, ch := range chains {
		if ch.Table.Name == "filter" {
			names = append(names, ch.Name)
		}
	}
	if len(names) > 0 {
		b, err := findIptables(ctx, "nf_tables")
		if err != nil {
			return nil, err
		}
		b.chains = names
		found = append(found, b)
	}
	if tables, err := os.ReadFile("/proc/thread-self/net/ip_tables_names"); err == nil && slices.Contains(strings.Fields(string(tables)), "filter") {
		if b, err := findIptables(ctx, "legacy"); err == nil {
			if out, err := runIptables(ctx, "", b.cmd, "-S"); err == nil {
				b.chains = chainNames(out)
				found = append(found, b)
			}
		}
	}
	return found, nil
}

// chainNames returns the chains of an "iptables -S" listing.
func chainNames(listing string) []string {
	var names []string
	for line := range strings.Lines(listing) {
		f := strings.Fields(line)
		if len(f) >= 2 && (f[0] == "-P" || f[0] == "-N") {
			names = append(names, f[1])
		}
	}
	return names
}

// findIptables finds the command of a backend: "nf_tables" or "legacy", as
// "iptables --version" names them.
func findIptables(ctx context.Context, backend string) (iptables, error) {
	variant := map[string]string{"nf_tables": "iptables-nft", "legacy": "iptables-legacy"}[backend]
	if _, err := exec.LookPath(variant); err == nil {
		return iptables{cmd: variant, restore: variant + "-restore"}, nil
	}
	out, err := exec.CommandContext(ctx, "iptables", "--version").Output()
	if err == nil && (strings.Contains(string(out), "("+backend+")") ||
		backend == "legacy" && !strings.Contains(string(out), "(")) {
		return iptables{cmd: "iptables", restore: "iptables-restore"}, nil
	}
	return iptables{}, fmt.Errorf("find iptables command for the %s backend: the filter table exists, but no command", backend)
}

// sync replaces the runplane rules of each chain of the backend with want,
// in one iptables-restore transaction. It changes no other rules.
func (b iptables) sync(ctx context.Context, want map[string][][]string) error {
	var in strings.Builder
	in.WriteString("*filter\n")
	for _, chain := range b.managed(want) {
		have, err := b.rules(ctx, chain)
		if err != nil {
			return err
		}
		for _, r := range have {
			fmt.Fprintf(&in, "-D %s\n", r)
		}
		for i, r := range want[chain] {
			fmt.Fprintf(&in, "-I %s %d %s\n", chain, i+1, strings.Join(r, " "))
		}
	}
	in.WriteString("COMMIT\n")
	if _, err := runIptables(ctx, in.String(), b.restore, "--noflush"); err != nil {
		return fmt.Errorf("write iptables rules: %w", err)
	}
	return nil
}

// inSync tells if each chain has as many runplane rules as want.
func (b iptables) inSync(ctx context.Context, want map[string][][]string) bool {
	for _, chain := range b.managed(want) {
		if have, err := b.rules(ctx, chain); err != nil || len(have) != len(want[chain]) {
			return false
		}
	}
	return true
}

// managed returns the chains of the backend that can have runplane rules.
// With a nil want, that is each of them, to remove the rules.
func (b iptables) managed(want map[string][][]string) []string {
	var list []string
	for _, chain := range RuleChains {
		if _, ok := want[chain]; (ok || want == nil) && slices.Contains(b.chains, chain) {
			list = append(list, chain)
		}
	}
	return list
}

// rules returns the runplane rules of a chain as "iptables -S" prints them,
// without the leading "-A".
func (b iptables) rules(ctx context.Context, chain string) ([]string, error) {
	out, err := runIptables(ctx, "", b.cmd, "-S", chain)
	if err != nil {
		return nil, fmt.Errorf("list %s rules: %w", chain, err)
	}
	var rules []string
	for line := range strings.Lines(out) {
		rule, ok := strings.CutPrefix(strings.TrimSpace(line), "-A ")
		if !ok {
			continue
		}
		fields := strings.Fields(rule)
		if i := slices.Index(fields, "--comment"); i >= 0 && i+1 < len(fields) && strings.Trim(fields[i+1], `"`) == RuleComment {
			rules = append(rules, rule)
		}
	}
	return rules, nil
}

// runIptables runs an iptables command. It waits for the xtables lock, but
// not for ever.
func runIptables(ctx context.Context, stdin, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, append([]string{"-w"}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s: %w: %s", name, err, bytes.TrimSpace(errOut.Bytes()))
	}
	return out.String(), nil
}
