package github

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/runplane/runplane/internal/ghrest"
)

func TestSignJWT(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	raw, err := signJWT(testKey(), testClientID, now)
	if err != nil {
		t.Fatal(err)
	}
	var claims jwt.RegisteredClaims
	_, err = jwt.ParseWithClaims(raw, &claims, func(*jwt.Token) (any, error) { return &testKey().PublicKey, nil },
		jwt.WithValidMethods([]string{"RS256"}), jwt.WithTimeFunc(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("parse JWT: %v", err)
	}
	// GitHub accepts at most 10 minutes from iat to exp.
	if claims.Issuer != testClientID || claims.IssuedAt.Unix() != 1_800_000_000-60 || claims.ExpiresAt.Unix() != 1_800_000_000+540 {
		t.Errorf("claims = iss %q, iat %d, exp %d; want 60 s before and 9 min after now", claims.Issuer,
			claims.IssuedAt.Unix(), claims.ExpiresAt.Unix())
	}
}

func TestInstallationToken(t *testing.T) {
	tests := []struct {
		name       string
		ttl        time.Duration
		refuse     string // the token that GitHub refuses
		wantCalls  int
		wantTokens int32
	}{
		{"reused while it lives longer than 5 minutes", time.Hour, "", 3, 1},
		{"renewed in its last 5 minutes", 4 * time.Minute, "", 3, 3},
		{"renewed once after GitHub refused it", time.Hour, "Bearer ghs_7_1", 4, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.connect()
			e.tokenTTL = tt.ttl
			calls := 0
			e.mux.HandleFunc("GET /api/v3/repos/acme/web", func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("Authorization") == tt.refuse {
					writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "Bad credentials"})
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{})
			})
			for range 3 {
				if err := e.svc.api.JSON(context.Background(), ghrest.Call{Method: http.MethodGet, Path: "/repos/acme/web", Auth: installAuth{e.svc, 7}}, nil); err != nil {
					t.Fatal(err)
				}
			}
			if calls != tt.wantCalls || e.tokens.Load() != tt.wantTokens {
				t.Errorf("calls = %d, tokens made = %d; want %d and %d", calls, e.tokens.Load(), tt.wantCalls, tt.wantTokens)
			}
		})
	}
}
