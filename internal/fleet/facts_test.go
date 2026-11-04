package fleet

import (
	"testing"

	"github.com/runplane/runplane/internal/agent"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/version"
)

func TestTokenWithoutNameNamesTheServerAfterItsHost(t *testing.T) {
	ctx := t.Context()
	h := newHarness(t, newFakeNode("arm64"))
	tok, err := h.f.NewToken(ctx, "https://ci.example.com", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if stored, err := h.st.GetServerToken(ctx, tok.ID); err != nil || stored.TokenHash != hashSecret(tok.Token) {
		t.Fatalf("token_id %q does not name the stored token: %+v, %v", tok.ID, stored, err)
	}
	id, _, err := h.f.Authenticate(ctx, agent.Hello{
		Token: tok.Token, Version: version.Version, Mode: sandbox.ModeContainer, Host: newFakeNode("x64").info, Addr: "198.51.100.7",
	})
	if err != nil {
		t.Fatal(err)
	}
	if v := h.server(id); v.Name != "node-x64" {
		t.Fatalf("server name %q, want the host name node-x64", v.Name)
	}
}

func TestRegionIsRecordedApartFromProvider(t *testing.T) {
	local := newFakeNode("arm64")
	local.info.Region = "fsn1"
	h := newHarness(t, local)
	h.run()
	node := newFakeNode("x64")
	node.info.Region = "nbg1"
	_, agentID := h.connectAgent("build-x64", node)

	regions, err := h.st.ServerRegions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if regions[h.localID()] != "fsn1" || regions[agentID] != "nbg1" {
		t.Fatalf("regions %v, want fsn1 for the primary and nbg1 for the agent", regions)
	}
	if v := h.server(agentID); v.Provider != "Hetzner" {
		t.Fatalf("provider %q, want Hetzner without the region", v.Provider)
	}
}
