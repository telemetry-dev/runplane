package fleet

import (
	"strings"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/agent"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
	"github.com/runplane/runplane/internal/version"
)

func TestInstallToken(t *testing.T) {
	ctx := t.Context()
	h := newHarness(t, newFakeNode("arm64"))
	tok, err := h.f.NewToken(ctx, "https://ci.example.com", "build-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok.Token, "rp_srv_") || !tok.ExpiresAt.Equal(h.clock.now().Add(time.Hour)) {
		t.Errorf("token %q expires at %v, want rp_srv_... for one hour", tok.Token, tok.ExpiresAt)
	}
	hello := agent.Hello{
		Token: tok.Token, Version: version.Version, Mode: sandbox.ModeContainer, Host: newFakeNode("x64").info, Addr: "198.51.100.7",
	}
	auth := func(hello agent.Hello) (string, string, error) { return h.f.Authenticate(ctx, hello) }
	id, secret, err := auth(hello)
	if err != nil || id == "" || secret == "" {
		t.Fatalf("register: id %q, secret %q, %v", id, secret, err)
	}
	v := h.server(id)
	if v.Name != "build-1" || v.Kind != store.ServerAgent || v.Status != store.ServerOnline || v.Arch != "x64" || v.Address != "198.51.100.7" {
		t.Errorf("new server row %+v", v)
	}
	// GET /api/servers/tokens/{token_id}/status follows the install.
	if stored, err := h.st.GetServerToken(ctx, tok.ID); err != nil || stored.ServerID != id || stored.UsedAt == nil {
		t.Errorf("token_id %q names %+v, %v; want the token that registered %s", tok.ID, stored, err, id)
	}

	if gotID, gotSecret, err := auth(agent.Hello{ServerID: id, Secret: secret}); err != nil || gotID != id || gotSecret != "" {
		t.Errorf("known agent: id %q, secret %q, %v; want its ID and no new secret", gotID, gotSecret, err)
	}
	if _, _, err := auth(agent.Hello{ServerID: id, Secret: secret + "x"}); err == nil {
		t.Error("wrong secret: no error")
	}
	other := hello
	other.Addr = "198.51.100.99"
	if _, _, err := auth(other); err == nil {
		t.Error("used token from another address: no error")
	}

	// The reply to the first use got lost, so the agent sends the token again.
	againID, again, err := auth(hello)
	if err != nil || againID != id || again == "" || again == secret {
		t.Fatalf("token again: id %q, secret %q, %v; want the same server and a new secret", againID, again, err)
	}
	if _, _, err := auth(agent.Hello{ServerID: id, Secret: secret}); err == nil {
		t.Error("the replaced secret still works")
	}
	if _, _, err := auth(agent.Hello{ServerID: id, Secret: again}); err != nil {
		t.Errorf("the new secret: %v", err)
	}
	h.clock.add(tokenGrace)
	if _, _, err := auth(hello); err == nil {
		t.Error("used token after the grace time: no error")
	}

	// A token without a name names the server after its host.
	unnamed, err := h.f.NewToken(ctx, "https://ci.example.com", "", "")
	if err != nil {
		t.Fatal(err)
	}
	hello.Token, hello.Addr = unnamed.Token, "198.51.100.8"
	unnamedID, _, err := auth(hello)
	if err != nil {
		t.Fatal(err)
	}
	if v := h.server(unnamedID); v.Name != "node-x64" {
		t.Errorf("server name %q of a token without a name, want the host name node-x64", v.Name)
	}

	late, err := h.f.NewToken(ctx, "https://ci.example.com", "build-2", "")
	if err != nil {
		t.Fatal(err)
	}
	h.clock.add(time.Hour)
	if _, _, err := auth(agent.Hello{Token: late.Token, Addr: "198.51.100.9"}); err == nil {
		t.Error("expired token: no error")
	}
}
