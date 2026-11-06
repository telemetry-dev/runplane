package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/runplane/runplane/internal/store"
)

func TestMoveJobs(t *testing.T) {
	tests := []struct {
		name, in, want string
		jobs           int
	}{
		{"ubuntu-latest", "    runs-on: ubuntu-latest\n", "    runs-on: big\n", 1},
		{"quoted ubuntu-24.04 with a comment", "    runs-on: 'ubuntu-24.04'   # pinned\n", "    runs-on: 'big'   # pinned\n", 1},
		{"CRLF line ends", "jobs:\r\n  a:\r\n    runs-on: ubuntu-latest\r\n", "jobs:\r\n  a:\r\n    runs-on: big\r\n", 1},
		{"matrix entry", "        - runs-on: ubuntu-latest\n", "        - runs-on: big\n", 1},
		{"two jobs", "  a:\n    runs-on: ubuntu-latest\n  b:\n    runs-on: ubuntu-24.04", "  a:\n    runs-on: big\n  b:\n    runs-on: big", 2},
		{"other release", "    runs-on: ubuntu-22.04\n", "    runs-on: ubuntu-22.04\n", 0},
		{"larger runner", "    runs-on: ubuntu-latest-8-cores\n", "    runs-on: ubuntu-latest-8-cores\n", 0},
		{"expression", "    runs-on: ${{ matrix.os }}\n", "    runs-on: ${{ matrix.os }}\n", 0},
		{"label list", "    runs-on: [ubuntu-latest]\n", "    runs-on: [ubuntu-latest]\n", 0},
		{"other key", "        os: ubuntu-latest\n", "        os: ubuntu-latest\n", 0},
	}
	for _, tt := range tests {
		got, jobs := moveJobs(tt.in, "big")
		if got != tt.want || jobs != tt.jobs {
			t.Errorf("%s: moveJobs = %q, %d; want %q, %d", tt.name, got, jobs, tt.want, tt.jobs)
		}
	}
}

const ciWorkflow = `on: pull_request
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: make test
  lint:
    runs-on: "ubuntu-24.04" # pinned
  install:
    runs-on: ubuntu-22.04
`

const ciMoved = `on: pull_request
jobs:
  test:
    runs-on: big
    steps:
      - run: make test
  lint:
    runs-on: "big" # pinned
  install:
    runs-on: ubuntu-22.04
`

// fakeRepo serves the repository acme/web for OpenPullRequest and keeps
// the bodies of its writes by "METHOD path".
type fakeRepo struct {
	mu          sync.Mutex
	permissions map[string]string
	open        []map[string]any  // open pull requests of runplane-runs-on
	workflows   map[string]string // file name -> content; nil = no .github/workflows
	branchTaken bool              // runplane-runs-on exists
	writes      map[string]map[string]any
}

func (e *env) repo() *fakeRepo {
	e.t.Helper()
	ctx := context.Background()
	if err := e.st.ReplaceRepositories(ctx, 10, []store.Repository{
		{ID: 100, FullName: "acme/web", Private: true, DefaultBranch: "main", UpdatedAt: time.Now()},
	}); err != nil {
		e.t.Fatal(err)
	}
	f := &fakeRepo{
		permissions: map[string]string{"actions": "write", "contents": "write", "metadata": "read", "pull_requests": "write", "workflows": "write"},
		workflows:   map[string]string{"ci.yml": ciWorkflow, "release.yaml": "jobs:\n  build:\n    runs-on: macos-latest\n", "README.md": "runs-on: ubuntu-latest\n"},
		writes:      map[string]map[string]any{},
	}
	const repo = "/api/v3/repos/acme/web"
	m := e.mux
	m.HandleFunc("GET /api/v3/app/installations/10", func(w http.ResponseWriter, r *http.Request) {
		e.checkJWT(r)
		f.mu.Lock()
		defer f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"id": 10, "app_id": testAppID, "permissions": f.permissions,
			"account": map[string]any{"login": "acme", "id": 1, "type": "Organization"}})
	})
	m.HandleFunc("GET "+repo+"/pulls", func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query(); q.Get("head") != "acme:runplane-runs-on" || q.Get("state") != "open" {
			e.t.Errorf("pull request query %s, want the open pull requests of acme:runplane-runs-on", r.URL.RawQuery)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		writeJSON(w, http.StatusOK, f.open)
	})
	m.HandleFunc("GET "+repo+"/branches/main", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"name": "main",
			"commit": map[string]any{"sha": "c1", "commit": map[string]any{"tree": map[string]any{"sha": "t1"}}}})
	})
	m.HandleFunc("GET "+repo+"/contents/.github/workflows", func(w http.ResponseWriter, r *http.Request) {
		if ref := r.URL.Query().Get("ref"); ref != "c1" {
			e.t.Errorf("workflows read at %q, want the head of main", ref)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.workflows == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
			return
		}
		entries := []map[string]any{{"type": "dir", "name": "scripts", "path": ".github/workflows/scripts", "sha": "d1"}}
		for name := range f.workflows {
			entries = append(entries, map[string]any{"type": "file", "name": name, "path": ".github/workflows/" + name, "sha": "blob-" + name})
		}
		writeJSON(w, http.StatusOK, entries)
	})
	m.HandleFunc("GET "+repo+"/git/blobs/{sha}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		content := f.workflows[strings.TrimPrefix(r.PathValue("sha"), "blob-")]
		f.mu.Unlock()
		// GitHub breaks the base64 text into lines of 60 characters.
		enc := base64.StdEncoding.EncodeToString([]byte(content))
		var lines []string
		for len(enc) > 60 {
			lines, enc = append(lines, enc[:60]), enc[60:]
		}
		writeJSON(w, http.StatusOK, map[string]any{"content": strings.Join(append(lines, enc), "\n") + "\n", "encoding": "base64"})
	})
	write := func(status int, out map[string]any) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.writes[r.Method+" "+strings.TrimPrefix(r.URL.Path, repo)] = body
			taken := f.branchTaken
			f.mu.Unlock()
			if r.URL.Path == repo+"/git/refs" && taken {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "Reference already exists"})
				return
			}
			writeJSON(w, status, out)
		}
	}
	m.HandleFunc("POST "+repo+"/git/trees", write(http.StatusCreated, map[string]any{"sha": "t2"}))
	m.HandleFunc("POST "+repo+"/git/commits", write(http.StatusCreated, map[string]any{"sha": "c2"}))
	m.HandleFunc("POST "+repo+"/git/refs", write(http.StatusCreated, map[string]any{"ref": "refs/heads/runplane-runs-on"}))
	m.HandleFunc("PATCH "+repo+"/git/refs/heads/runplane-runs-on", write(http.StatusOK, map[string]any{"ref": "refs/heads/runplane-runs-on"}))
	m.HandleFunc("POST "+repo+"/pulls", write(http.StatusCreated, map[string]any{"number": 12, "html_url": "https://github.com/acme/web/pull/12"}))
	return f
}

func (f *fakeRepo) written() map[string]map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes
}

func TestOpenPullRequest(t *testing.T) {
	ctx := context.Background()

	t.Run("commits the moved jobs to a new branch", func(t *testing.T) {
		e := acmeEnv(t)
		f := e.repo()
		number, url, err := e.svc.OpenPullRequest(ctx, "https://ci.example.com", "acme/web", "big")
		if err != nil || number != 12 || url != "https://github.com/acme/web/pull/12" {
			t.Fatalf("OpenPullRequest = %d, %q, %v; want pull request 12", number, url, err)
		}
		w := f.written()
		wantTree := map[string]any{"base_tree": "t1", "tree": []any{map[string]any{
			"path": ".github/workflows/ci.yml", "mode": "100644", "type": "blob", "content": ciMoved,
		}}}
		if got := w["POST /git/trees"]; !reflect.DeepEqual(got, wantTree) {
			t.Errorf("tree = %v, want %v", got, wantTree)
		}
		if got := w["POST /git/commits"]; got["tree"] != "t2" || !reflect.DeepEqual(got["parents"], []any{"c1"}) {
			t.Errorf("commit = %v, want tree t2 on c1", got)
		}
		if got := w["POST /git/refs"]; got["ref"] != "refs/heads/runplane-runs-on" || got["sha"] != "c2" {
			t.Errorf("ref = %v, want runplane-runs-on at c2", got)
		}
		if got := w["POST /pulls"]; got["head"] != "runplane-runs-on" || got["base"] != "main" {
			t.Errorf("pull request = %v, want runplane-runs-on into main", got)
		}
	})

	t.Run("gives the open pull request of an earlier call", func(t *testing.T) {
		e := acmeEnv(t)
		f := e.repo()
		f.open = []map[string]any{{"number": 7, "html_url": "https://github.com/acme/web/pull/7"}}
		number, url, err := e.svc.OpenPullRequest(ctx, "https://ci.example.com", "acme/web", "big")
		if err != nil || number != 7 || url != "https://github.com/acme/web/pull/7" {
			t.Errorf("OpenPullRequest = %d, %q, %v; want the open pull request 7", number, url, err)
		}
		if w := f.written(); len(w) != 0 {
			t.Errorf("writes = %v, want none", w)
		}
	})

	t.Run("moves the branch of a closed pull request", func(t *testing.T) {
		e := acmeEnv(t)
		f := e.repo()
		f.branchTaken = true
		if _, _, err := e.svc.OpenPullRequest(ctx, "https://ci.example.com", "acme/web", "big"); err != nil {
			t.Fatal(err)
		}
		w := f.written()
		if got := w["PATCH /git/refs/heads/runplane-runs-on"]; got["sha"] != "c2" || got["force"] != true {
			t.Errorf("ref update = %v, want a forced move to c2", got)
		}
		if w["POST /pulls"] == nil {
			t.Error("no pull request after the branch moved")
		}
	})

	errorTests := []struct {
		name       string
		repository string
		change     func(f *fakeRepo)
		wantStatus int
	}{
		{"unknown repository", "acme/other", func(*fakeRepo) {}, http.StatusNotFound},
		{"no workflows permission", "acme/web", func(f *fakeRepo) { delete(f.permissions, "workflows") }, http.StatusForbidden},
		{"read-only contents", "acme/web", func(f *fakeRepo) { f.permissions["contents"] = "read" }, http.StatusForbidden},
		{"no workflows", "acme/web", func(f *fakeRepo) { f.workflows = nil }, http.StatusUnprocessableEntity},
		{"no job on ubuntu-latest", "acme/web", func(f *fakeRepo) { delete(f.workflows, "ci.yml") }, http.StatusUnprocessableEntity},
	}
	for _, tt := range errorTests {
		t.Run(tt.name, func(t *testing.T) {
			e := acmeEnv(t)
			f := e.repo()
			tt.change(f)
			_, _, err := e.svc.OpenPullRequest(ctx, "https://ci.example.com", tt.repository, "big")
			var se huma.StatusError
			if !errors.As(err, &se) || se.GetStatus() != tt.wantStatus {
				t.Errorf("err = %v, want status %d", err, tt.wantStatus)
			}
			if w := f.written(); len(w) != 0 {
				t.Errorf("writes = %v, want none", w)
			}
		})
	}
}
