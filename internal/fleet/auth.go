package fleet

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"time"

	"github.com/runplane/runplane/internal/agent"
	"github.com/runplane/runplane/internal/ids"
	"github.com/runplane/runplane/internal/store"
)

const (
	tokenPrefix = "rp_srv_"
	tokenTTL    = time.Hour
	// tokenGrace is how long an agent can use its token again after the
	// reply to its first use got lost.
	tokenGrace = 5 * time.Minute
)

var (
	errBadToken  = errors.New("the install token is not valid. A token works one time and for one hour. Make a new install command on the Servers page")
	errBadSecret = errors.New("unknown server ID or wrong secret. If the server was removed, install the agent again with a new token")
)

// usedToken is a token that registered an agent.
type usedToken struct {
	serverID string
	addr     string
	at       time.Time
}

// Authenticate checks the hello of an agent: the Authenticate hook of
// agent.Listener. A new agent registers with its install token and gets a
// secret. A known agent keeps its secret.
func (f *Fleet) Authenticate(ctx context.Context, h agent.Hello) (serverID, secret string, err error) {
	switch {
	case h.Token != "":
		return f.register(ctx, h)
	case h.ServerID != "" && h.Secret != "":
		return f.checkSecret(ctx, h)
	}
	return "", "", errors.New("the agent sent no install token and no server ID")
}

func (f *Fleet) register(ctx context.Context, h agent.Hello) (string, string, error) {
	hash := hashSecret(h.Token)
	secret := rand.Text()
	now := f.now()
	v, err := f.st.ConsumeServerToken(ctx, hash, now, store.Server{
		ID: ids.New(), Kind: store.ServerAgent, Status: store.ServerOnline,
		ServerFacts: serverFacts(h.Host, h.Mode, h.Version, h.Addr),
		SecretHash:  hashSecret(secret), LastSeenAt: &now, CreatedAt: now, UpdatedAt: now,
	})
	if errors.Is(err, store.ErrNotFound) {
		return f.registerAgain(ctx, hash, h.Addr)
	}
	if err != nil {
		f.log.Error("register agent", "addr", h.Addr, "err", err)
		return "", "", errors.New("the primary could not record the server; see its log")
	}
	// A token made without a name names the server after its host.
	if v.Name == "" {
		v.Name = cmp.Or(h.Host.Hostname, "agent-"+v.ID[len(v.ID)-6:])
		if err := f.st.SetServerName(ctx, v.ID, v.Name, now); err != nil {
			f.log.Warn("name the new server", "server_id", v.ID, "err", err)
		}
	}
	f.mu.Lock()
	for k, u := range f.tokens {
		if now.Sub(u.at) >= tokenGrace {
			delete(f.tokens, k)
		}
	}
	f.tokens[hash] = usedToken{serverID: v.ID, addr: h.Addr, at: now}
	f.mu.Unlock()
	f.log.Info("server registered", "server_id", v.ID, "name", v.Name, "addr", h.Addr)
	return v.ID, secret, nil
}

// registerAgain accepts a token a second time from the agent that used it a
// short time ago: the agent did not get the reply, so it does not know its
// server ID. The agent gets a new secret.
func (f *Fleet) registerAgain(ctx context.Context, hash, addr string) (string, string, error) {
	f.mu.Lock()
	u, ok := f.tokens[hash]
	removed := f.removed[u.serverID]
	f.mu.Unlock()
	if !ok || removed || u.addr != addr || f.now().Sub(u.at) >= tokenGrace {
		return "", "", errBadToken
	}
	secret := rand.Text()
	if err := f.st.SetServerSecretHash(ctx, u.serverID, hashSecret(secret), f.now()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", "", errBadToken
		}
		f.log.Error("register agent again", "server_id", u.serverID, "err", err)
		return "", "", errors.New("the primary could not record the server; see its log")
	}
	f.log.Info("server registered again with its token", "server_id", u.serverID, "addr", addr)
	return u.serverID, secret, nil
}

func (f *Fleet) checkSecret(ctx context.Context, h agent.Hello) (string, string, error) {
	f.mu.Lock()
	removed := f.removed[h.ServerID]
	f.mu.Unlock()
	if removed {
		return "", "", errBadSecret
	}
	v, err := f.st.GetServer(ctx, h.ServerID)
	if errors.Is(err, store.ErrNotFound) {
		return "", "", errBadSecret
	}
	if err != nil {
		f.log.Error("read server of an agent", "server_id", h.ServerID, "err", err)
		return "", "", errors.New("the primary could not read the server; see its log")
	}
	if v.Kind != store.ServerAgent || subtle.ConstantTimeCompare([]byte(v.SecretHash), []byte(hashSecret(h.Secret))) != 1 {
		return "", "", errBadSecret
	}
	return v.ID, "", nil
}

// Token is a one-time install token and the command that installs an agent
// with it.
type Token struct {
	ID        string
	Token     string
	Command   string
	ExpiresAt time.Time
}

// NewToken makes a one-time install token for a new agent. base is the URL
// of the dashboard, and an empty name means the host name of the agent.
func (f *Fleet) NewToken(ctx context.Context, base, name, createdBy string) (Token, error) {
	token, t, err := f.newToken(ctx, name, createdBy, "")
	if err != nil {
		return Token{}, err
	}
	return Token{ID: t.ID, Token: token, Command: f.installCommand(base, token), ExpiresAt: t.ExpiresAt}, nil
}

// newToken records a new install token. serverID is the server row that the
// token fills in, or "" for a new row.
func (f *Fleet) newToken(ctx context.Context, name, createdBy, serverID string) (string, store.ServerToken, error) {
	token := tokenPrefix + rand.Text()
	now := f.now()
	t := store.ServerToken{
		ID: ids.New(), TokenHash: hashSecret(token), Name: name, CreatedBy: createdBy,
		CreatedAt: now, ExpiresAt: now.Add(tokenTTL), ServerID: serverID,
	}
	if err := f.st.CreateServerToken(ctx, t); err != nil {
		return "", store.ServerToken{}, err
	}
	return token, t, nil
}

// hashSecret is the stored form of a token or a secret. Both are random, so
// a plain SHA-256 is sufficient.
func hashSecret(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
