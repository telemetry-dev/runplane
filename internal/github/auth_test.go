package github

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// GitHub tokens live one hour. runplane uses a token again until 5 minutes
// before its end, and asks for a new one after GitHub refuses it. The fake
// GitHub checks the app JWT of each new token (refer to checkJWT).
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
		{"renewed once after GitHub refused it", time.Hour, "Bearer ghs_10_1", 4, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := acmeEnv(t)
			e.tokenTTL = tt.ttl
			calls := 0
			e.mux.HandleFunc("GET /api/v3/repos/acme/web/actions/runs/555", func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("Authorization") == tt.refuse {
					writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "Bad credentials"})
					return
				}
				writeJSON(w, http.StatusOK, runJSON("push", true, 100))
			})
			for range 3 {
				if _, err := e.svc.CheckJob(context.Background(), "acme/web", 555); err != nil {
					t.Fatal(err)
				}
			}
			if calls != tt.wantCalls || e.tokens.Load() != tt.wantTokens {
				t.Errorf("calls = %d, tokens made = %d; want %d and %d", calls, e.tokens.Load(), tt.wantCalls, tt.wantTokens)
			}
		})
	}
}
