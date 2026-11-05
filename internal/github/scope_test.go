package github

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/ghrest"
	"github.com/runplane/runplane/internal/store"
)

// The runner deprecations API answers only for an organization or a
// repository, so a personal account needs one of its repositories.
func TestReadScope(t *testing.T) {
	tests := []struct {
		name      string
		installs  func(e *env)
		wantPath  string
		wantToken string // the installation token of the path
	}{
		{"no installation", func(*env) {}, "", ""},
		{"personal account without repositories", func(e *env) { e.install(20, "octo", "User") }, "", ""},
		{"personal account", func(e *env) {
			e.install(20, "octo", "User")
			e.repos(20, "octo/app")
		}, "/repos/octo/app", "ghs_20_1"},
		{"an organization comes first", func(e *env) {
			e.install(20, "octo", "User")
			e.repos(20, "octo/app")
			e.install(10, "acme", "Organization")
		}, "/orgs/acme", "ghs_10_1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.connect()
			tt.installs(e)
			path, auth, ok := e.svc.ReadScope(context.Background())
			if path != tt.wantPath || ok != (tt.wantPath != "") {
				t.Fatalf("ReadScope = %q, %v; want %q", path, ok, tt.wantPath)
			}
			if !ok {
				return
			}
			var got string
			e.mux.HandleFunc("GET /api/v3/scope-check", func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("Authorization")
				writeJSON(w, http.StatusOK, map[string]any{})
			})
			if err := e.svc.api.JSON(context.Background(), ghrest.Call{Method: http.MethodGet, Path: "/scope-check", Auth: auth}, &struct{}{}); err != nil {
				t.Fatal(err)
			}
			if got != "Bearer "+tt.wantToken {
				t.Errorf("Authorization = %q, want the token of the installation %q", got, tt.wantToken)
			}
		})
	}
}

func (e *env) repos(installation int64, names ...string) {
	e.t.Helper()
	var list []store.Repository
	for i, n := range names {
		list = append(list, store.Repository{ID: installation*100 + int64(i), InstallationID: installation, FullName: n, UpdatedAt: time.Now()})
	}
	if err := e.st.ReplaceRepositories(context.Background(), installation, list); err != nil {
		e.t.Fatal(err)
	}
}
