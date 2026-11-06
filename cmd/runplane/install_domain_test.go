package main

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/runplane/runplane"
	"github.com/runplane/runplane/internal/installtest"
)

// install.sh checks --domain before it stops the primary, and set-domain
// checks the domain after the stop. A domain that only set-domain refuses
// thus leaves runplane stopped. set-domain owns the rule.
func TestInstallDomainAgreesWithSetDomain(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RUNPLANE_CONFIG_DIR", dir)
	t.Setenv("RUNPLANE_DATA_DIR", dir)
	label := strings.Repeat("a", 63)
	domains := []string{
		"",
		"ci.example.com",
		"CI.Example.com",
		"a.b",
		"xn--bcher-kva.example",
		label + ".com",
		"a" + label + ".com",
		label + "." + label + "." + label + "." + label[:61], // 253 characters
		label + "." + label + "." + label + "." + label[:62], // 254 characters
		"localhost",
		"192.0.2.1",
		"0.0.0.0",
		"255.255.255.255",
		// net.ParseIP reads these as host names, not IP addresses.
		"192.0.2.01",
		"192.0.2.256",
		"192.0.2.1.5",
		"ci..example.com",
		"ci.example.com.",
		"-ci.example.com",
		"ci-.example.com",
		"bücher.example",
		"ci.example.com:443",
		"https://ci.example.com",
	}
	script, err := installtest.ReplaceMain(runplane.InstallScript,
		`for d; do if (parse_flags --domain "$d") >/dev/null 2>&1; then echo true; else echo false; fi; done`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := installtest.Sh(script, nil, append([]string{"-s", "--"}, domains...)...)
	got := strings.Fields(out)
	if err != nil || len(got) != len(domains) {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
	for i, d := range domains {
		want := strconv.FormatBool(setDomain(context.Background(), []string{d}) == nil)
		if got[i] != want {
			t.Errorf("install.sh accepts --domain %q: %s, set-domain accepts it: %s. Change install.sh and internal/settings/clean.go together.", d, got[i], want)
		}
	}
}
