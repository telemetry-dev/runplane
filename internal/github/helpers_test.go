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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/golang-jwt/jwt/v5"

	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/ghrest"
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

// env is a Service with a real store and an httptest GitHub. The REST API
// is under /api/v3 as on GitHub Enterprise Server, because the scaleset
// library uses that path for each host other than github.com.
type env struct {
	t        *testing.T
	st       *store.Store
	box      *secret.Box
	bus      *events.Bus
	mux      *http.ServeMux
	srv      *httptest.Server
	svc      *Service
	sched    *fakeScheduler
	inst     *fakeInstallations // the installations that Sync reads
	tokens   atomic.Int32       // installation tokens that the fake made
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
	e := &env{t: t, st: st, box: box, bus: &events.Bus{}, mux: http.NewServeMux(), tokenTTL: time.Hour,
		sched: &fakeScheduler{got: make(chan scheduler.Message, 16)}}
	e.srv = httptest.NewServer(e.mux)
	t.Cleanup(e.srv.Close)
	e.mux.HandleFunc("POST /api/v3/app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		e.checkJWT(r)
		writeJSON(w, http.StatusCreated, map[string]any{
			"token":      fmt.Sprintf("ghs_%s_%d", r.PathValue("id"), e.tokens.Add(1)),
			"expires_at": time.Now().Add(e.tokenTTL),
		})
	})
	e.inst = e.installations()
	e.svc = New(Config{Store: st, Box: box, Bus: e.bus, Log: slog.New(slog.DiscardHandler), Scheduler: e.sched})
	// Config has no option for the GitHub hosts, so the test sets them.
	e.svc.web = e.srv.URL
	e.svc.api = ghrest.Client{URL: e.srv.URL + "/api/v3", RetryWait: time.Millisecond}
	t.Cleanup(func() {
		e.svc.stop()
		e.svc.stopListeners()
	})
	return e
}

// sync reads the installations of the fake and makes the scale sets agree.
func (e *env) sync() {
	e.t.Helper()
	if err := e.svc.Sync(context.Background()); err != nil {
		e.t.Fatal(err)
	}
}

// connect stores the GitHub App, as the manifest flow does.
func (e *env) connect() {
	e.t.Helper()
	err := e.st.SaveGitHubApp(context.Background(), store.GitHubApp{
		ID: "app1", AppID: testAppID, Slug: "runplane-test", ClientID: testClientID,
		OwnerLogin: "acme", OwnerType: "Organization", PrivateKeyEnc: e.box.Seal(testPEM()), CreatedAt: time.Now(),
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

// acmeEnv is connected and installed on the organization acme (ID 10).
func acmeEnv(t *testing.T) *env {
	e := newEnv(t)
	e.connect()
	e.install(10, "acme", "Organization")
	return e
}

// install records the installation in the store, and in the fake, so that
// a Sync keeps it.
func (e *env) install(id int64, login, accountType string) {
	e.t.Helper()
	err := e.st.UpsertInstallation(context.Background(), store.Installation{
		ID: id, AppID: testAppID, AccountLogin: login, AccountType: accountType, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	if err != nil {
		e.t.Fatal(err)
	}
	e.inst.mu.Lock()
	e.inst.list = append(e.inst.list, map[string]any{"id": id, "app_id": testAppID, "suspended_at": nil,
		"account": map[string]any{"login": login, "id": id, "type": accountType}})
	e.inst.mu.Unlock()
}

func (e *env) pool(id, label string, enabled bool) {
	e.t.Helper()
	err := e.st.CreatePool(context.Background(), store.Pool{
		ID: id, Name: label, Label: label, Enabled: enabled, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

// checkJWT accepts only an app JWT that GitHub accepts: RS256 with the key
// of the app and the client ID as the issuer, an issue time that is not in
// the future, and at most 10 minutes from the issue time to the expiry.
func (e *env) checkJWT(r *http.Request) {
	raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	var claims jwt.RegisteredClaims
	_, err := jwt.ParseWithClaims(raw, &claims, func(*jwt.Token) (any, error) { return &testKey().PublicKey, nil },
		jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(testClientID), jwt.WithIssuedAt(), jwt.WithExpirationRequired())
	switch {
	case err != nil:
	case claims.IssuedAt == nil:
		err = fmt.Errorf("no iat")
	case claims.ExpiresAt.Sub(claims.IssuedAt.Time) > 10*time.Minute:
		err = fmt.Errorf("iat %s and exp %s are more than 10 minutes apart", claims.IssuedAt, claims.ExpiresAt)
	}
	if err != nil {
		e.t.Errorf("%s %s: bad app JWT: %v", r.Method, r.URL.Path, err)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// countEvents counts the events on ch so far.
func countEvents[E any](ch <-chan E) int {
	n := 0
	for {
		select {
		case <-ch:
			n++
		default:
			return n
		}
	}
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
// for jobs that it accepts. It sends the messages with jobs to got.
type fakeScheduler struct {
	last     scheduler.ScaleSetClient // of the last attached scale set
	detached []string
	max      int
	got      chan scheduler.Message

	mu       sync.Mutex
	acquired []int64 // the keys that AcquireJobs gave back
}

func (f *fakeScheduler) Attach(_ store.ScaleSet, c scheduler.ScaleSetClient) scheduler.Scaler {
	f.last = c
	c.SetMaxRunners(f.max)
	return scalerFunc(func(ctx context.Context, m scheduler.Message) error {
		if len(m.Available) == 0 {
			return nil
		}
		var keys []int64
		for _, j := range m.Available {
			keys = append(keys, j.RunnerRequestID)
		}
		acquired, err := c.AcquireJobs(ctx, keys)
		if err != nil {
			return err
		}
		f.mu.Lock()
		f.acquired = append(f.acquired, acquired...)
		f.mu.Unlock()
		f.got <- m
		return nil
	})
}

func (f *fakeScheduler) Detach(id string) { f.detached = append(f.detached, id) }

type scalerFunc func(context.Context, scheduler.Message) error

func (f scalerFunc) Scale(ctx context.Context, m scheduler.Message) error {
	return f(ctx, m)
}

// fakeActions serves the scale set endpoints that the scaleset library
// calls: the registration token, the Actions service, and the message queue.
type fakeActions struct {
	mu sync.Mutex
	actionsState
	nextID     int
	conflict   bool             // the session endpoint answers 409
	noUpdate   bool             // the update of a scale set fails
	agents     map[int64]string // runner ID -> "busy", "gone", or "broken"; others are removable
	revokeNext bool             // the next admin token is revoked at once
	revoked    string
	queue      chan string
}

// actionsState is the part of fakeActions that tests check.
type actionsState struct {
	sets     map[int]scaleset.RunnerScaleSet
	scopes   map[int]string // scale set ID -> "acme" or "octo--app"
	sessions int            // open sessions
	closed   int            // deleted sessions
	deleted  []int          // deleted scale sets
	acquired []int64
	polls    []int // the max capacity of each poll
	removed  []int64
}

func (f *fakeActions) state() actionsState {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.actionsState
	s.sets, s.scopes = maps.Clone(s.sets), maps.Clone(s.scopes)
	return s
}

func (e *env) actions() *fakeActions {
	f := &fakeActions{actionsState: actionsState{sets: map[int]scaleset.RunnerScaleSet{}, scopes: map[int]string{}},
		nextID: 7, agents: map[int64]string{}, queue: make(chan string, 8)}
	m := e.mux
	regToken := func(w http.ResponseWriter, r *http.Request) {
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
		// The scaleset library reads the expiry of the admin token.
		admin, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)), ID: rand.Text(),
		}).SignedString([]byte("admin"))
		f.mu.Lock()
		if f.revokeNext {
			f.revoked, f.revokeNext = admin, false
		}
		f.mu.Unlock()
		scope := strings.ReplaceAll(strings.Trim(u.Path, "/"), "/", "--")
		writeJSON(w, http.StatusOK, map[string]any{"url": e.srv.URL + "/actions/" + scope + "/", "token": admin})
	})
	const sets = "/actions/{scope}/_apis/runtime/runnerscalesets"
	m.HandleFunc("GET "+sets, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("Authorization") == "Bearer "+f.revoked {
			actionsError(w, http.StatusUnauthorized, "VssUnauthorizedException")
			return
		}
		var found []scaleset.RunnerScaleSet
		for id, s := range f.sets {
			if f.scopes[id] == r.PathValue("scope") && s.Name == r.URL.Query().Get("name") &&
				strconv.Itoa(s.RunnerGroupID) == r.URL.Query().Get("runnerGroupId") {
				found = append(found, s)
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
		f.sets[s.ID], f.scopes[s.ID] = s, r.PathValue("scope")
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, s)
	})
	m.HandleFunc("PATCH "+sets+"/{id}", func(w http.ResponseWriter, r *http.Request) {
		var s scaleset.RunnerScaleSet
		json.NewDecoder(r.Body).Decode(&s)
		s.ID, _ = strconv.Atoi(r.PathValue("id"))
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.noUpdate {
			actionsError(w, http.StatusBadRequest, "InvalidRunnerScaleSetException")
			return
		}
		f.sets[s.ID] = s
		writeJSON(w, http.StatusOK, s)
	})
	m.HandleFunc("DELETE "+sets+"/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.Atoi(r.PathValue("id"))
		f.mu.Lock()
		delete(f.sets, id)
		f.deleted = append(f.deleted, id)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	m.HandleFunc("POST "+sets+"/{id}/sessions", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.Atoi(r.PathValue("id"))
		f.mu.Lock()
		defer f.mu.Unlock()
		set, ok := f.sets[id]
		switch {
		case f.conflict:
			actionsError(w, http.StatusConflict, "TaskAgentSessionConflictException")
		case !ok:
			actionsError(w, http.StatusNotFound, "RunnerScaleSetNotFoundException")
		default:
			f.sessions++
			writeJSON(w, http.StatusOK, map[string]any{
				"sessionId": fmt.Sprintf("8b1f2c4e-0000-4000-8000-%012d", id), "ownerName": "test-host", "runnerScaleSet": set,
				"messageQueueUrl": e.srv.URL + "/queue/" + r.PathValue("id"), "messageQueueAccessToken": "mq",
				"statistics": map[string]int{"totalRegisteredRunners": 1},
			})
		}
	})
	m.HandleFunc("DELETE "+sets+"/{id}/sessions/{sid}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.sessions--
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
		writeJSON(w, http.StatusOK, map[string]any{
			"runner": map[string]any{"id": 9001, "name": s.Name}, "encodedJITConfig": "jit-" + s.Name + "-" + s.WorkFolder,
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
			actionsError(w, http.StatusBadRequest, "JobStillRunningException")
		case "gone":
			actionsError(w, http.StatusNotFound, "AgentNotFoundException")
		case "broken":
			actionsError(w, http.StatusNotImplemented, "Exception")
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	m.HandleFunc("GET /queue/{id}", func(w http.ResponseWriter, r *http.Request) {
		capacity, _ := strconv.Atoi(r.Header.Get(scaleset.HeaderScaleSetMaxCapacity))
		f.mu.Lock()
		f.polls = append(f.polls, capacity)
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

// actionsError writes an exception of the Actions service.
func actionsError(w http.ResponseWriter, status int, typeName string) {
	writeJSON(w, status, map[string]any{"typeName": "GitHub.DistributedTask.WebApi." + typeName, "message": typeName})
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
