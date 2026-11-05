//go:build linux

package netrules

import (
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"slices"
	"testing"
	"time"
)

func TestAllowlistKeepsAddressesUntilTheyExpire(t *testing.T) {
	m := New(slog.New(slog.NewTextHandler(io.Discard, nil)), netip.Prefix{})
	cfg := Config{Allowlist: []string{"GitHub.com.", "140.82.112.0/20", "203.0.113.7", "*.example.com", "::1"}}
	addrs := func(s ...string) []netip.Addr {
		var a []netip.Addr
		for _, v := range s {
			a = append(a, netip.MustParseAddr(v))
		}
		return a
	}
	check := func(step string, want ...string) {
		t.Helper()
		var got []string
		for _, p := range m.allowed(cfg) {
			got = append(got, p.String())
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: allowed = %v, want %v", step, got, want)
		}
	}
	t0 := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

	m.remember(cfg, map[string][]netip.Addr{"github.com": addrs("20.87.245.0")}, nil, t0)
	check("first answer", "20.87.245.0/32", "140.82.112.0/20", "203.0.113.7/32")

	m.remember(cfg, map[string][]netip.Addr{"github.com": addrs("20.87.245.4")}, nil, t0.Add(10*time.Minute))
	check("rotated answer", "20.87.245.0/32", "20.87.245.4/32", "140.82.112.0/20", "203.0.113.7/32")

	m.remember(cfg, nil, map[string]error{"github.com": errors.New("timeout")}, t0.Add(20*time.Minute))
	check("failed lookup", "20.87.245.0/32", "20.87.245.4/32", "140.82.112.0/20", "203.0.113.7/32")

	m.remember(cfg, map[string][]netip.Addr{"github.com": addrs("20.87.245.4")}, nil, t0.Add(61*time.Minute))
	check("after an hour", "20.87.245.4/32", "140.82.112.0/20", "203.0.113.7/32")

	cfg.Allowlist = []string{"140.82.112.0/20"}
	m.remember(cfg, nil, nil, t0.Add(62*time.Minute))
	check("name removed", "140.82.112.0/20")
}
