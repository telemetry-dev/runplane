package github

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestSignJWTVerifiesWithAppPublicKey(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	raw, err := signJWT(testKey(), testClientID, now)
	if err != nil {
		t.Fatal(err)
	}
	var claims jwt.RegisteredClaims
	tok, err := jwt.ParseWithClaims(raw, &claims, func(*jwt.Token) (any, error) { return &testKey().PublicKey, nil },
		jwt.WithValidMethods([]string{"RS256"}), jwt.WithTimeFunc(func() time.Time { return now }))
	if err != nil || !tok.Valid {
		t.Fatalf("parse JWT: %v", err)
	}
	if claims.Issuer != testClientID {
		t.Errorf("iss = %q, want %q", claims.Issuer, testClientID)
	}
	if got := claims.IssuedAt.Unix(); got != 1_800_000_000-60 {
		t.Errorf("iat = %d, want 60 s before now", got)
	}
	if got := claims.ExpiresAt.Unix(); got != 1_800_000_000+540 {
		t.Errorf("exp = %d, want 9 min after now", got)
	}
}

func TestInstallationTokenCache(t *testing.T) {
	tests := []struct {
		name       string
		ttl        time.Duration
		wantTokens int32
	}{
		{"reused while it lives longer than 5 minutes", time.Hour, 1},
		{"renewed in its last 5 minutes", 4 * time.Minute, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.connect()
			e.tokenTTL = tt.ttl
			e.mux.HandleFunc("GET /api/v3/repos/acme/web", func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ghs_7_") {
					t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
				}
				writeJSON(w, http.StatusOK, map[string]any{"id": 1})
			})
			for range 3 {
				if err := e.svc.api.json(context.Background(), call{method: http.MethodGet, path: "/repos/acme/web", auth: installAuth{e.svc, 7}}, nil); err != nil {
					t.Fatal(err)
				}
			}
			if got := e.tokens.Load(); got != tt.wantTokens {
				t.Errorf("tokens made = %d, want %d", got, tt.wantTokens)
			}
		})
	}
}

func TestRefusedTokenIsRenewedOnce(t *testing.T) {
	e := newEnv(t)
	e.connect()
	calls := 0
	e.mux.HandleFunc("GET /api/v3/repos/acme/web", func(w http.ResponseWriter, r *http.Request) {
		calls++
		// GitHub revoked the first token.
		if r.Header.Get("Authorization") == "Bearer ghs_7_1" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "Bad credentials"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{})
	})
	err := e.svc.api.json(context.Background(), call{method: http.MethodGet, path: "/repos/acme/web", auth: installAuth{e.svc, 7}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || e.tokens.Load() != 2 {
		t.Errorf("calls = %d, tokens = %d; want 2 and 2", calls, e.tokens.Load())
	}
}
