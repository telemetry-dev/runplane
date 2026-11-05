package github

import (
	"cmp"
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/runplane/runplane/internal/ghrest"
	"github.com/runplane/runplane/internal/store"
)

var errNotConnected = errors.New("runplane has no GitHub App: connect GitHub first")

// creds is the GitHub App with its opened private key.
type creds struct {
	app store.GitHubApp
	pem string
	key *rsa.PrivateKey
}

// issuer is the JWT issuer. GitHub accepts the client ID or the app ID and
// recommends the client ID.
func (c *creds) issuer() string {
	return cmp.Or(c.app.ClientID, strconv.FormatInt(c.app.AppID, 10))
}

// creds returns the app from the store, cached after the first read.
func (s *Service) creds(ctx context.Context) (*creds, error) {
	s.credMu.Lock()
	defer s.credMu.Unlock()
	if s.cred != nil {
		return s.cred, nil
	}
	app, err := s.st.GetGitHubApp(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errNotConnected
	}
	if err != nil {
		return nil, err
	}
	raw, err := s.box.Open(app.PrivateKeyEnc)
	if err != nil {
		return nil, fmt.Errorf("open GitHub App key: %w", err)
	}
	key, err := parseKey(raw)
	if err != nil {
		return nil, err
	}
	s.cred = &creds{app: app, pem: string(raw), key: key}
	return s.cred, nil
}

func (s *Service) dropCreds() {
	s.credMu.Lock()
	s.cred = nil
	s.credMu.Unlock()
	s.tokMu.Lock()
	clear(s.tokens)
	s.tokMu.Unlock()
}

func parseKey(raw []byte) (*rsa.PrivateKey, error) {
	key, err := jwt.ParseRSAPrivateKeyFromPEM(raw)
	if err != nil {
		return nil, fmt.Errorf("parse GitHub App key: %w", err)
	}
	return key, nil
}

// signJWT makes the RS256 JWT that authenticates runplane as the app. The
// issue time is one minute back for clock drift; GitHub accepts at most 10
// minutes of lifetime.
func signJWT(key *rsa.PrivateKey, issuer string, now time.Time) (string, error) {
	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(now.Add(-time.Minute)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
		Issuer:    issuer,
	}).SignedString(key)
	if err != nil {
		return "", fmt.Errorf("sign GitHub App JWT: %w", err)
	}
	return signed, nil
}

type appAuth struct{ s *Service }

func (a appAuth) Header(ctx context.Context) (string, error) {
	c, err := a.s.creds(ctx)
	if err != nil {
		return "", err
	}
	signed, err := signJWT(c.key, c.issuer(), time.Now())
	if err != nil {
		return "", err
	}
	return "Bearer " + signed, nil
}

type installAuth struct {
	s  *Service
	id int64
}

func (a installAuth) Header(ctx context.Context) (string, error) {
	t, err := a.s.installationToken(ctx, a.id)
	if err != nil {
		return "", err
	}
	return "Bearer " + t, nil
}

func (a installAuth) Invalidate() {
	a.s.tokMu.Lock()
	delete(a.s.tokens, a.id)
	a.s.tokMu.Unlock()
}

type token struct {
	value   string
	expires time.Time
}

// installationToken returns a cached installation token or makes a new one.
// GitHub tokens live one hour; runplane stops using a token 5 minutes
// before it expires.
func (s *Service) installationToken(ctx context.Context, installationID int64) (string, error) {
	s.tokMu.Lock()
	defer s.tokMu.Unlock()
	if t, ok := s.tokens[installationID]; ok && time.Until(t.expires) > 5*time.Minute {
		return t.value, nil
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	err := s.api.JSON(ctx, ghrest.Call{
		Method: http.MethodPost,
		Path:   fmt.Sprintf("/app/installations/%d/access_tokens", installationID),
		Auth:   appAuth{s},
	}, &out)
	if err != nil {
		return "", fmt.Errorf("make installation token: %w", err)
	}
	s.tokens[installationID] = token{out.Token, out.ExpiresAt}
	return out.Token, nil
}
