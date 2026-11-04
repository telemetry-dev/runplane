package github

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/runplane/runplane/internal/store"
)

// ErrNotConnected means that runplane has no GitHub App yet.
var ErrNotConnected = errors.New("runplane has no GitHub App: connect GitHub first")

// creds is the GitHub App with its opened private key.
type creds struct {
	app store.GitHubApp
	pem string
	key *rsa.PrivateKey
}

// issuer is the JWT issuer. GitHub accepts the client ID or the app ID and
// recommends the client ID.
func (c *creds) issuer() string {
	if c.app.ClientID != "" {
		return c.app.ClientID
	}
	return strconv.FormatInt(c.app.AppID, 10)
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
		return nil, ErrNotConnected
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

// dropCreds drops the cached app and its installation tokens.
func (s *Service) dropCreds() {
	s.credMu.Lock()
	s.cred = nil
	s.credMu.Unlock()
	s.tokMu.Lock()
	clear(s.tokens)
	s.tokMu.Unlock()
}

func parseKey(raw []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("parse GitHub App key: no PEM block")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse GitHub App key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("parse GitHub App key: not an RSA key")
	}
	return key, nil
}

// signJWT makes the RS256 JWT that authenticates runplane as the app. The
// issue time is one minute back for clock drift; GitHub accepts at most 10
// minutes of lifetime.
func signJWT(key *rsa.PrivateKey, issuer string, now time.Time) (string, error) {
	enc := base64.RawURLEncoding
	claims, err := json.Marshal(struct {
		IssuedAt  int64  `json:"iat"`
		ExpiresAt int64  `json:"exp"`
		Issuer    string `json:"iss"`
	}{now.Add(-time.Minute).Unix(), now.Add(9 * time.Minute).Unix(), issuer})
	if err != nil {
		return "", err
	}
	unsigned := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(nil, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("sign GitHub App JWT: %w", err)
	}
	return unsigned + "." + enc.EncodeToString(sig), nil
}

// appAuth authenticates as the GitHub App.
type appAuth struct{ s *Service }

func (a appAuth) header(ctx context.Context) (string, error) {
	c, err := a.s.creds(ctx)
	if err != nil {
		return "", err
	}
	jwt, err := signJWT(c.key, c.issuer(), time.Now())
	if err != nil {
		return "", err
	}
	return "Bearer " + jwt, nil
}

func (appAuth) invalidate() {}

// installAuth authenticates as one installation of the app.
type installAuth struct {
	s  *Service
	id int64
}

func (a installAuth) header(ctx context.Context) (string, error) {
	t, err := a.s.installationToken(ctx, a.id)
	if err != nil {
		return "", err
	}
	return "Bearer " + t, nil
}

func (a installAuth) invalidate() {
	a.s.tokMu.Lock()
	delete(a.s.tokens, a.id)
	a.s.tokMu.Unlock()
}

type token struct {
	value   string
	expires time.Time
}

// tokenMargin is how long before its expiry runplane stops using a token.
const tokenMargin = 5 * time.Minute

// installationToken returns a cached installation token or makes a new one.
// GitHub tokens live one hour.
func (s *Service) installationToken(ctx context.Context, installationID int64) (string, error) {
	s.tokMu.Lock()
	defer s.tokMu.Unlock()
	if t, ok := s.tokens[installationID]; ok && time.Until(t.expires) > tokenMargin {
		return t.value, nil
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	err := s.api.json(ctx, call{
		method: http.MethodPost,
		path:   fmt.Sprintf("/app/installations/%d/access_tokens", installationID),
		auth:   appAuth{s},
	}, &out)
	if err != nil {
		return "", fmt.Errorf("make installation token: %w", err)
	}
	s.tokens[installationID] = token{out.Token, out.ExpiresAt}
	return out.Token, nil
}
