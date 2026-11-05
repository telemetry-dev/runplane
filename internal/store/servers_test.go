package store

import (
	"errors"
	"slices"
	"testing"
	"time"
)

func TestConsumeServerToken(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	now := time.Now()
	must(t, s.CreateServer(ctx, Server{ID: "s-ssh", Name: "edge-1", Kind: ServerAgent, Status: ServerInstalling,
		CreatedAt: now, UpdatedAt: now}))
	for _, tok := range []ServerToken{
		{ID: "t1", TokenHash: "h-new", Name: "build-2", CreatedAt: now, ExpiresAt: now.Add(time.Hour)},
		{ID: "t2", TokenHash: "h-ssh", Name: "ignored", ServerID: "s-ssh", CreatedAt: now, ExpiresAt: now.Add(time.Hour)},
		{ID: "t3", TokenHash: "h-old", Name: "old", CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)},
	} {
		must(t, s.CreateServerToken(ctx, tok))
	}
	agent := func(id string) Server {
		return Server{ID: id, Kind: ServerAgent, Status: ServerOnline, SecretHash: "secret-" + id,
			ServerFacts: ServerFacts{Arch: "arm64", VCPU: 8}, CreatedAt: now, UpdatedAt: now}
	}

	// A token makes a new server with the name of the token.
	if got, err := s.ConsumeServerToken(ctx, "h-new", now, agent("s1")); err != nil || got.ID != "s1" || got.Name != "build-2" {
		t.Fatalf("new server = %+v, %v; want s1 with the token name", got, err)
	}
	// A token made before an SSH install fills its server, which keeps its ID and name.
	got, err := s.ConsumeServerToken(ctx, "h-ssh", now, agent("s2"))
	if err != nil || got.ID != "s-ssh" || got.Name != "edge-1" || got.Status != ServerOnline ||
		got.SecretHash != "secret-s2" || got.VCPU != 8 {
		t.Fatalf("SSH server = %+v, %v; want s-ssh online with the facts of s2", got, err)
	}
	for _, hash := range []string{"h-new", "h-old", "h-unknown"} { // used, expired, unknown
		if _, err := s.ConsumeServerToken(ctx, hash, now, agent("s3")); !errors.Is(err, ErrNotFound) {
			t.Errorf("token %s: err = %v, want ErrNotFound", hash, err)
		}
	}

	list, err := s.ListServers(ctx)
	must(t, err)
	var servers []string
	for _, v := range list {
		servers = append(servers, v.ID+" "+v.SecretHash)
	}
	if want := []string{"s1 secret-s1", "s-ssh secret-s2"}; !slices.Equal(servers, want) {
		t.Fatalf("servers = %q, want %q", servers, want)
	}
}

// A deleted server leaves no ID in the pools, so a later pool change does
// not fail on an unknown server.
func TestDeleteServerLeavesThePools(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	now := time.Now()
	for _, id := range []string{"s1", "s2"} {
		must(t, s.CreateServer(ctx, Server{ID: id, Name: id, Kind: ServerAgent, Status: ServerOnline, CreatedAt: now, UpdatedAt: now}))
	}
	tests := []struct {
		label   string
		ids     []string
		want    []string
		enabled bool
	}{
		{"both", []string{"s1", "s2"}, []string{"s2"}, true},
		{"only-s1", []string{"s1"}, []string{}, false}, // empty would mean all servers
		{"all", []string{}, []string{}, true},
		{"only-s2", []string{"s2"}, []string{"s2"}, true},
	}
	for _, tt := range tests {
		err := s.CreatePool(ctx, Pool{ID: tt.label, Name: tt.label, Label: tt.label, Arch: "x64", VCPU: 2, MemoryMB: 4096,
			DiskGB: 64, MaxRunners: 2, ServerIDs: tt.ids, Enabled: true, CreatedAt: now, UpdatedAt: now})
		must(t, err)
	}

	must(t, s.DeleteServer(ctx, "s1"))
	for _, tt := range tests {
		p, err := s.GetPool(ctx, tt.label)
		must(t, err)
		if !slices.Equal(p.ServerIDs, tt.want) || p.Enabled != tt.enabled {
			t.Errorf("pool %s: server_ids %q, enabled %v; want %q, %v", tt.label, p.ServerIDs, p.Enabled, tt.want, tt.enabled)
		}
	}
	if err := s.DeleteServer(ctx, "s1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: err = %v, want ErrNotFound", err)
	}
}

func TestDoctorResultReplacesTheLastOne(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Millisecond)
	must(t, s.CreateServer(ctx, Server{ID: "srv", Name: "srv", Kind: ServerAgent, Status: ServerOnline, CreatedAt: now, UpdatedAt: now}))
	if _, err := s.GetDoctorResult(ctx, "srv"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a server without a doctor run: err = %v, want ErrNotFound", err)
	}
	for i, status := range []string{"fail", "ok"} {
		err := s.SaveDoctorResult(ctx, DoctorResult{ServerID: "srv", CheckedAt: now.Add(time.Duration(i) * time.Minute),
			Checks: []DoctorCheck{{ID: "clock", Status: status}}})
		must(t, err)
	}
	r, err := s.GetDoctorResult(ctx, "srv")
	if err != nil || !r.CheckedAt.Equal(now.Add(time.Minute)) || len(r.Checks) != 1 || r.Checks[0].Status != "ok" {
		t.Fatalf("doctor result %+v, %v; want the second run", r, err)
	}
	// The result goes with its server.
	must(t, s.DeleteServer(ctx, "srv"))
	if _, err := s.GetDoctorResult(ctx, "srv"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("doctor result after the server is deleted: err = %v, want ErrNotFound", err)
	}
}
