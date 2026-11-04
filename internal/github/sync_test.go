package github

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/store"
)

// fakeInstallations serves /app/installations in pages of one (at most
// two pages), and the repositories of each installation.
type fakeInstallations struct {
	mu    sync.Mutex
	list  []map[string]any
	repos map[string][]map[string]any // by installation token prefix "ghs_<id>_"
}

func (e *env) installations() *fakeInstallations {
	f := &fakeInstallations{repos: map[string][]map[string]any{}}
	e.mux.HandleFunc("GET /api/v3/app/installations", func(w http.ResponseWriter, r *http.Request) {
		e.checkJWT(r)
		f.mu.Lock()
		defer f.mu.Unlock()
		page := 0
		if r.URL.Query().Get("page") == "2" {
			page = 1
		}
		var out []map[string]any
		if page < len(f.list) {
			out = f.list[page : page+1]
		}
		if page == 0 && len(f.list) > 1 {
			w.Header().Set("Link", `<`+e.srv.URL+`/api/v3/app/installations?per_page=100&page=2>; rel="next"`)
		}
		writeJSON(w, http.StatusOK, out)
	})
	e.mux.HandleFunc("GET /api/v3/installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		for prefix, repos := range f.repos {
			if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "+prefix) {
				writeJSON(w, http.StatusOK, map[string]any{"total_count": len(repos), "repositories": repos})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"total_count": 0, "repositories": []any{}})
	})
	return f
}

func (f *fakeInstallations) set(list []map[string]any, repos map[string][]map[string]any) {
	f.mu.Lock()
	f.list, f.repos = list, repos
	f.mu.Unlock()
}

func TestSyncInstallations(t *testing.T) {
	e := newEnv(t)
	e.connect()
	ctx := context.Background()
	f := e.installations()
	org := map[string]any{"id": 10, "app_id": testAppID, "repository_selection": "all", "suspended_at": nil,
		"account": map[string]any{"login": "acme", "id": 1, "type": "Organization"}}
	user := map[string]any{"id": 20, "app_id": testAppID, "repository_selection": "selected", "suspended_at": nil,
		"account": map[string]any{"login": "octo", "id": 2, "type": "User"}}
	f.set([]map[string]any{org, user}, map[string][]map[string]any{
		"ghs_10_": {{"id": 100, "full_name": "acme/web", "private": true}, {"id": 101, "full_name": "acme/site", "private": false}},
		"ghs_20_": {{"id": 200, "full_name": "octo/app", "private": true, "archived": true}},
	})

	if err := e.svc.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	insts, _ := e.st.ListInstallations(ctx)
	if len(insts) != 2 {
		t.Fatalf("installations = %+v, want acme and octo (two pages)", insts)
	}
	repos, _ := e.st.ListRepositories(ctx, 10)
	if len(repos) != 2 || repos[1].FullName != "acme/web" || !repos[1].Private || repos[0].Private {
		t.Errorf("acme repositories = %+v", repos)
	}
	if repos, _ := e.st.ListRepositories(ctx, 20); len(repos) != 1 || !repos[0].Archived {
		t.Errorf("octo repositories = %+v", repos)
	}

	// A scale set of the user installation that goes away. runplane cannot
	// reach it any more, so only the row goes.
	repoID := int64(200)
	if err := e.st.UpsertScaleSet(ctx, store.ScaleSet{ID: "ss1", PoolID: "p1", InstallationID: 20, RepositoryID: &repoID,
		GitHubID: 5, Name: "runplane", Status: store.ScaleSetListening, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	// acme is suspended and loses a repository; octo uninstalled the app.
	suspended := "2026-11-01T08:00:00Z"
	org["suspended_at"] = suspended
	f.set([]map[string]any{org}, map[string][]map[string]any{"ghs_10_": {{"id": 100, "full_name": "acme/web", "private": true}}})
	if err := e.svc.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	inst, err := e.st.GetInstallation(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if inst.SuspendedAt == nil || !inst.SuspendedAt.Equal(time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("suspended_at = %v", inst.SuspendedAt)
	}
	if repos, _ := e.st.ListRepositories(ctx, 10); len(repos) != 2 {
		t.Errorf("a suspended installation keeps its known repositories, got %+v", repos)
	}
	if _, err := e.st.GetInstallation(ctx, 20); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the removed installation is still there: %v", err)
	}
	if repos, _ := e.st.ListRepositories(ctx, 20); len(repos) != 0 {
		t.Errorf("repositories of the removed installation: %+v", repos)
	}
	if _, err := e.st.GetScaleSet(ctx, "ss1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the scale set of the removed installation is still there: %v", err)
	}

	// The suspension ends.
	org["suspended_at"] = nil
	f.set([]map[string]any{org}, map[string][]map[string]any{"ghs_10_": {{"id": 100, "full_name": "acme/web", "private": true}}})
	if err := e.svc.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	inst, _ = e.st.GetInstallation(ctx, 10)
	repos, _ = e.st.ListRepositories(ctx, 10)
	if inst.SuspendedAt != nil || len(repos) != 1 {
		t.Errorf("after the suspension: suspended_at = %v, repositories = %+v", inst.SuspendedAt, repos)
	}
}
