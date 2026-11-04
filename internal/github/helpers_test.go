package github

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/golang-jwt/jwt/v5"

	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/scheduler"
	"github.com/runplane/runplane/internal/secret"
	"github.com/runplane/runplane/internal/store"
)

const (
	testAppID    = 42
	testClientID = "Iv23.testclient"
)

var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

func testPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(testKey())})
}

// env is a Service with a real store and an httptest GitHub.
type env struct {
	t     *testing.T
	st    *store.Store
	box   *secret.Box
	bus   *events.Bus
	mux   *http.ServeMux
	srv   *httptest.Server
	svc   *Service
	sched *fakeScheduler

	tokens   atomic.Int32 // installation tokens that the fake made
	tokenTTL time.Duration
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "runplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, err := secret.NewBox(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, st: st, box: box, bus: &events.Bus{}, mux: http.NewServeMux(), tokenTTL: time.Hour}
	e.sched = &fakeScheduler{clients: map[string]scheduler.ScaleSetClient{}, got: make(chan *scaleset.RunnerScaleSetMessage, 16)}
	e.srv = httptest.NewServer(e.mux)
	t.Cleanup(e.srv.Close)
	e.mux.HandleFunc("POST /api/v3/app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		e.checkJWT(r)
		n := e.tokens.Add(1)
		writeJSON(w, http.StatusCreated, map[string]any{
			"token":      fmt.Sprintf("ghs_%s_%d", r.PathValue("id"), n),
			"expires_at": time.Now().Add(e.tokenTTL).UTC().Format(time.RFC3339),
		})
	})
	e.svc = New(Config{
		Store: st, Box: box, Bus: e.bus, Scheduler: e.sched, HTTP: e.srv.Client(),
		APIURL: e.srv.URL + "/api/v3", WebURL: e.srv.URL, Owner: "test-host",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	t.Cleanup(func() {
		e.svc.stop()
		e.svc.stopListeners()
	})
	return e
}

// connect stores the GitHub App, as the manifest flow does.
func (e *env) connect() {
	e.t.Helper()
	err := e.st.SaveGitHubApp(context.Background(), store.GitHubApp{
		ID: "app1", AppID: testAppID, Slug: "runplane-test", Name: "runplane-test", ClientID: testClientID,
		OwnerLogin: "acme", OwnerType: "Organization", PrivateKeyEnc: e.box.Seal(testPEM()), CreatedAt: time.Now(),
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) installOrg(id int64, login string) {
	e.t.Helper()
	e.install(store.Installation{ID: id, AppID: testAppID, AccountLogin: login, AccountType: "Organization", RepositorySelection: "all"})
}

func (e *env) install(inst store.Installation) {
	e.t.Helper()
	inst.CreatedAt, inst.UpdatedAt = time.Now(), time.Now()
	if err := e.st.UpsertInstallation(context.Background(), inst); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) pool(id, label string, enabled bool) store.Pool {
	e.t.Helper()
	p := store.Pool{ID: id, Name: label, Label: label, Arch: "x64", VCPU: 2, MemoryMB: 4096, DiskGB: 20,
		MaxRunners: 5, Enabled: enabled, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := e.st.CreatePool(context.Background(), p); err != nil {
		e.t.Fatal(err)
	}
	return p
}

// checkJWT verifies an app JWT with the public key of the test app.
func (e *env) checkJWT(r *http.Request) {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		e.t.Errorf("%s %s: no bearer token", r.Method, r.URL.Path)
		return
	}
	tok, err := jwt.Parse(raw, func(*jwt.Token) (any, error) { return &testKey().PublicKey, nil },
		jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(testClientID))
	if err != nil || !tok.Valid {
		e.t.Errorf("%s %s: bad app JWT: %v", r.Method, r.URL.Path, err)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// eventually waits up to 10 seconds for cond.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// fakeScheduler acquires every available job, like the real scheduler does
// for jobs that it accepts. It sends the messages from GitHub to got.
type fakeScheduler struct {
	mu       sync.Mutex
	clients  map[string]scheduler.ScaleSetClient
	detached []string
	max      int
	got      chan *scaleset.RunnerScaleSetMessage
}

func (f *fakeScheduler) Attach(set store.ScaleSet, c scheduler.ScaleSetClient) listener.Scaler {
	f.mu.Lock()
	f.clients[set.ID] = c
	max := f.max
	f.mu.Unlock()
	c.SetMaxRunners(max)
	return scalerFunc(func(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) error {
		if msg != nil && len(msg.JobAvailableMessages) > 0 {
			var ids []int64
			for _, j := range msg.JobAvailableMessages {
				ids = append(ids, j.RunnerRequestID)
			}
			if _, err := c.AcquireJobs(ctx, ids); err != nil {
				return err
			}
		}
		if msg != nil && msg.MessageID != listener.InitialMessageID {
			select {
			case f.got <- msg:
			default:
			}
		}
		return nil
	})
}

func (f *fakeScheduler) Detach(id string) {
	f.mu.Lock()
	f.detached = append(f.detached, id)
	f.mu.Unlock()
}

func (f *fakeScheduler) client(id string) scheduler.ScaleSetClient {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clients[id]
}

type scalerFunc func(context.Context, *scaleset.RunnerScaleSetMessage) error

func (f scalerFunc) Scale(ctx context.Context, m *scaleset.RunnerScaleSetMessage) error {
	return f(ctx, m)
}

// fakeActions serves the scale set endpoints that the scaleset library
// calls: the registration token, the Actions service, and the message queue.
type fakeActions struct {
	e  *env
	mu sync.Mutex

	sets        map[int]*scaleset.RunnerScaleSet
	scopes      map[int]string // scale set ID -> "acme" or "octo--app"
	nextID      int
	conflict    bool           // the session endpoint answers 409
	sessions    map[int]string // open session per scale set
	closed      int            // deleted sessions
	deletedSets []int
	acquired    []int64
	polls       []int // the max capacity of each poll
	queue       chan string
	agents      map[int64]string // runner ID -> "busy" or "gone"; others are removable
	removed     []int64
	revokeNext  bool            // the next admin token is revoked at once
	revoked     map[string]bool // admin tokens that get 401
	admins      int             // admin tokens issued
}

func (e *env) actions() *fakeActions {
	f := &fakeActions{e: e, sets: map[int]*scaleset.RunnerScaleSet{}, scopes: map[int]string{}, nextID: 7, sessions: map[int]string{},
		queue: make(chan string, 8), agents: map[int64]string{}, revoked: map[string]bool{}}
	m := e.mux
	regToken := func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ghs_") {
			e.t.Errorf("registration token without installation token")
		}
		writeJSON(w, http.StatusCreated, map[string]any{"token": "reg", "expires_at": time.Now().Add(time.Hour)})
	}
	m.HandleFunc("POST /api/v3/orgs/{org}/actions/runners/registration-token", regToken)
	m.HandleFunc("POST /api/v3/repos/{owner}/{repo}/actions/runners/registration-token", regToken)
	// Like GitHub, the Actions service URL is per organization or
	// repository, so each scope has its own scale set names.
	m.HandleFunc("POST /api/v3/actions/runner-registration", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ URL string }
		json.NewDecoder(r.Body).Decode(&body)
		u, _ := url.Parse(body.URL)
		f.mu.Lock()
		f.admins++
		admin, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			ID:        strconv.Itoa(f.admins),
		}).SignedString([]byte("admin"))
		if f.revokeNext {
			f.revoked[admin], f.revokeNext = true, false
		}
		f.mu.Unlock()
		scope := strings.ReplaceAll(strings.Trim(u.Path, "/"), "/", "--")
		writeJSON(w, http.StatusOK, map[string]any{"url": e.srv.URL + "/actions/" + scope + "/", "token": admin})
	})
	const sets = "/actions/{scope}/_apis/runtime/runnerscalesets"
	m.HandleFunc("GET "+sets, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.revoked[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")] {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"typeName": "GitHub.DistributedTask.WebApi.VssUnauthorizedException",
				"message":  "The admin token is missing, wrong, or expired.",
			})
			return
		}
		var found []scaleset.RunnerScaleSet
		for id, s := range f.sets {
			if f.scopes[id] == r.PathValue("scope") && s.Name == r.URL.Query().Get("name") &&
				strconv.Itoa(s.RunnerGroupID) == r.URL.Query().Get("runnerGroupId") {
				found = append(found, *s)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"count": len(found), "value": found})
	})
	m.HandleFunc("POST "+sets, func(w http.ResponseWriter, r *http.Request) {
		var s scaleset.RunnerScaleSet
		json.NewDecoder(r.Body).Decode(&s)
		f.mu.Lock()
		s.ID = f.nextID
		f.nextID++
		f.sets[s.ID] = &s
		f.scopes[s.ID] = r.PathValue("scope")
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, s)
	})
	m.HandleFunc("PATCH "+sets+"/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.Atoi(r.PathValue("id"))
		var s scaleset.RunnerScaleSet
		json.NewDecoder(r.Body).Decode(&s)
		f.mu.Lock()
		defer f.mu.Unlock()
		s.ID = id
		f.sets[id] = &s
		writeJSON(w, http.StatusOK, s)
	})
	m.HandleFunc("DELETE "+sets+"/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.Atoi(r.PathValue("id"))
		f.mu.Lock()
		delete(f.sets, id)
		f.deletedSets = append(f.deletedSets, id)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	m.HandleFunc("POST "+sets+"/{id}/sessions", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.Atoi(r.PathValue("id"))
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.conflict {
			writeJSON(w, http.StatusConflict, map[string]any{
				"typeName": "GitHub.DistributedTask.WebApi.TaskAgentSessionConflictException",
				"message":  "The scale set already has an active session for owner other-host.",
			})
			return
		}
		if _, ok := f.sets[id]; !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{"typeName": "RunnerScaleSetNotFoundException", "message": "not found"})
			return
		}
		sid := fmt.Sprintf("8b1f2c4e-0000-4000-8000-%012d", id)
		f.sessions[id] = sid
		writeJSON(w, http.StatusOK, map[string]any{
			"sessionId": sid, "ownerName": "test-host", "runnerScaleSet": f.sets[id],
			"messageQueueUrl": e.srv.URL + "/queue/" + r.PathValue("id"), "messageQueueAccessToken": "mq",
			"statistics": map[string]int{"totalAssignedJobs": 0, "totalRegisteredRunners": 1},
		})
	})
	m.HandleFunc("DELETE "+sets+"/{id}/sessions/{sid}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.Atoi(r.PathValue("id"))
		f.mu.Lock()
		delete(f.sessions, id)
		f.closed++
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	m.HandleFunc("POST "+sets+"/{id}/acquirejobs", func(w http.ResponseWriter, r *http.Request) {
		var ids []int64
		json.NewDecoder(r.Body).Decode(&ids)
		f.mu.Lock()
		f.acquired = append(f.acquired, ids...)
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"count": len(ids), "value": ids})
	})
	m.HandleFunc("POST "+sets+"/{id}/generatejitconfig", func(w http.ResponseWriter, r *http.Request) {
		var s scaleset.RunnerScaleSetJitRunnerSetting
		json.NewDecoder(r.Body).Decode(&s)
		if s.WorkFolder != "_work" {
			e.t.Errorf("work folder = %q", s.WorkFolder)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"runner": map[string]any{"id": 9001, "name": s.Name}, "encodedJITConfig": "jit-" + s.Name,
		})
	})
	m.HandleFunc("DELETE /actions/{scope}/_apis/distributedtask/pools/0/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		f.mu.Lock()
		state := f.agents[id]
		f.removed = append(f.removed, id)
		f.mu.Unlock()
		switch state {
		case "busy":
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"typeName": "GitHub.DistributedTask.WebApi.JobStillRunningException", "message": "The runner is running a job.",
			})
		case "gone":
			writeJSON(w, http.StatusNotFound, map[string]any{
				"typeName": "GitHub.DistributedTask.WebApi.AgentNotFoundException", "message": "No agent found.",
			})
		case "broken":
			writeJSON(w, http.StatusNotImplemented, map[string]any{"typeName": "Exception", "message": "down"})
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	m.HandleFunc("GET /queue/{id}", func(w http.ResponseWriter, r *http.Request) {
		max, _ := strconv.Atoi(r.Header.Get(scaleset.HeaderScaleSetMaxCapacity))
		f.mu.Lock()
		f.polls = append(f.polls, max)
		f.mu.Unlock()
		select {
		case body := <-f.queue:
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, body)
		case <-time.After(20 * time.Millisecond):
			w.WriteHeader(http.StatusAccepted)
		case <-r.Context().Done():
		}
	})
	m.HandleFunc("DELETE /queue/{id}/{mid}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	return f
}

// send queues a message with one JobAvailable for the next poll.
func (f *fakeActions) send(messageID int, requestID int64) {
	jobs, _ := json.Marshal([]map[string]any{{
		"messageType": "JobAvailable", "runnerRequestId": requestID, "repositoryName": "web", "ownerName": "acme",
		"jobDisplayName": "build", "workflowRunId": 555, "eventName": "push", "requestLabels": []string{"runplane"},
	}})
	body, _ := json.Marshal(map[string]any{
		"messageId": messageID, "messageType": "RunnerScaleSetJobMessages", "body": string(jobs),
		"statistics": map[string]int{"totalAvailableJobs": 1, "totalAssignedJobs": 1, "totalRegisteredRunners": 2, "totalIdleRunners": 2},
	})
	f.queue <- string(body)
}

// actionsState is a copy of the fake state for assertions.
type actionsState struct {
	sets        map[int]scaleset.RunnerScaleSet
	scopes      map[int]string
	sessions    map[int]string
	closed      int
	deletedSets []int
	acquired    []int64
	polls       []int
	removed     []int64
}

func (f *fakeActions) state() actionsState {
	f.mu.Lock()
	defer f.mu.Unlock()
	sets := map[int]scaleset.RunnerScaleSet{}
	for k, v := range f.sets {
		sets[k] = *v
	}
	return actionsState{
		sets: sets, scopes: maps.Clone(f.scopes), sessions: maps.Clone(f.sessions), closed: f.closed, deletedSets: slices.Clone(f.deletedSets),
		acquired: slices.Clone(f.acquired), polls: slices.Clone(f.polls), removed: slices.Clone(f.removed),
	}
}
