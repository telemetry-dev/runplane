// Package fakegh is a fake GitHub for end-to-end tests and local demos. It
// serves the REST calls of a GitHub App installation, the runner scale set
// API that github.com/actions/scaleset calls, the actions/runner releases,
// and a small protocol for the fake runner in testdata/fakerunner. Tests
// queue jobs with QueueJob and read the results with Jobs and Runners.
//
// URLs: with the server at http://host:port, the scale set config URL of
// the organization acme is http://host:port/acme. The scaleset client sees
// a GitHub Enterprise Server host there and sends REST calls to /api/v3. The
// server also serves REST at the root path, as api.github.com does, so that
// Transport can send calls for github.com and api.github.com here.
//
// The server accepts any well-formed app JWT and keeps all state in memory.
package fakegh

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// The app that the server knows. Any app ID or client ID works in a JWT.
const (
	AppID       = 1001
	AppClientID = "Iv23liFakeGitHubApp"
)

// Config configures a Server. The zero value works.
type Config struct {
	// Installations of the app. Default: DefaultInstallations, the
	// organization acme with the private repositories acme/api and acme/web
	// and the public acme/site.
	Installations []Installation
	// RunnerURL is the server URL that fake runners use, for example
	// http://host.docker.internal:9100 for runners in containers. Default:
	// the URL of the request that made the JIT config.
	RunnerURL string
	// RunnerDir is the fake runner bundle (testdata/fakerunner). The server
	// serves it as the archive of each actions/runner release. Empty:
	// release downloads return 404.
	RunnerDir string
	// RunnerVersion is the latest actions/runner release. Default "2.330.0".
	RunnerVersion string
	// PollTimeout is how long a message poll waits for a message. GitHub
	// waits about 50 seconds. Default 10 seconds.
	PollTimeout time.Duration
	// SessionTimeout is how long a message session lives without a poll.
	// Until then, a new session for the same scale set gets 409 Conflict.
	// Default 1 minute.
	SessionTimeout time.Duration
	// RunnerTimeout is how long a busy runner can be silent. Then the
	// server fails its job, as GitHub does when it loses a runner. Default
	// 1 minute.
	RunnerTimeout time.Duration
	Logger        *slog.Logger
}

type Installation struct {
	ID          int64  `json:"id"`
	Account     string `json:"account"`      // login of the organization or user
	AccountType string `json:"account_type"` // "Organization" or "User"
	Selected    bool   `json:"selected"`     // repository_selection "selected", else "all"
	Repos       []Repo `json:"repos"`
}

type Repo struct {
	Name    string `json:"name"` // "owner/name"
	Private bool   `json:"private"`
}

// DefaultInstallations are the installations of a Server with no
// Config.Installations.
func DefaultInstallations() []Installation {
	return []Installation{{
		ID:          1,
		Account:     "acme",
		AccountType: "Organization",
		Repos:       []Repo{{"acme/api", true}, {"acme/web", true}, {"acme/site", false}},
	}}
}

// Server is the fake GitHub. It is an http.Handler, so httptest.NewServer
// can serve it.
type Server struct {
	cfg     Config
	keyPEM  []byte
	log     *slog.Logger
	mux     *http.ServeMux
	closed  chan struct{}
	close   sync.Once
	archive archive

	mu       sync.Mutex
	wake     chan struct{} // closed and replaced at each change, for long polls
	app      app
	installs []*installation
	repos    map[string]*repo // by lower-case full name
	tokens   map[string]*token
	codes    map[string]manifest // app manifest codes
	sets     map[int]*scaleSet
	sessions map[uuid.UUID]*session
	runners  map[int64]*runner
	jobs     []*job // in request ID order
	runs     map[int64]*run
	runSeq   map[string]int64 // last run number by repository and workflow
	broken   []string         // scale set names whose JIT configs are not valid
	last     struct {
		set                   int
		runner, request, repo int64
		run, job              int64
	}
}

func New(cfg Config) *Server {
	if cfg.Installations == nil {
		cfg.Installations = DefaultInstallations()
	}
	if cfg.RunnerVersion == "" {
		cfg.RunnerVersion = "2.330.0"
	}
	if cfg.PollTimeout <= 0 {
		cfg.PollTimeout = 10 * time.Second
	}
	if cfg.SessionTimeout <= 0 {
		cfg.SessionTimeout = time.Minute
	}
	if cfg.RunnerTimeout <= 0 {
		cfg.RunnerTimeout = time.Minute
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err) // only for a bad key size
	}
	s := &Server{
		cfg:      cfg,
		keyPEM:   pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}),
		log:      cfg.Logger,
		mux:      http.NewServeMux(),
		closed:   make(chan struct{}),
		wake:     make(chan struct{}),
		app:      app{name: "runplane", slug: "runplane"},
		repos:    map[string]*repo{},
		tokens:   map[string]*token{},
		codes:    map[string]manifest{},
		sets:     map[int]*scaleSet{},
		sessions: map[uuid.UUID]*session{},
		runners:  map[int64]*runner{},
		runs:     map[int64]*run{},
		runSeq:   map[string]int64{},
	}
	s.last.request = 1000
	s.last.runner = 100
	s.last.repo = 700_000_000
	// Real run and job IDs do not fit in 32 bits.
	s.last.run = 18_000_000_000
	s.last.job = 52_000_000_000
	for _, in := range cfg.Installations {
		inst := &installation{Installation: in}
		s.installs = append(s.installs, inst)
		for _, r := range in.Repos {
			s.addRepo(inst, r)
		}
	}
	s.routes()
	go s.watchRunners()
	return s
}

// PrivateKey returns the PEM private key of the app. The server does not
// check JWT signatures, so any RSA key works too.
func (s *Server) PrivateKey() []byte { return s.keyPEM }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.log.Debug("request", "method", r.Method, "path", r.URL.Path)
	s.mux.ServeHTTP(w, r)
}

// Serve serves the fake on addr until ctx ends.
func (s *Server) Serve(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s, ReadHeaderTimeout: 10 * time.Second}
	stop := context.AfterFunc(ctx, func() {
		s.Close()
		srv.Shutdown(context.Background())
	})
	defer stop()
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Close ends the open long polls and the runner watch. Call it before
// httptest.Server.Close, which waits for open requests.
func (s *Server) Close() {
	s.close.Do(func() { close(s.closed) })
}

// Transport returns an http.RoundTripper that sends requests for
// github.com and api.github.com to the fake at baseURL and all other
// requests to http.DefaultTransport. With it, code that has the GitHub URLs
// built in talks to the fake.
func Transport(baseURL string) (http.RoundTripper, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("fakegh: bad base URL %q", baseURL)
	}
	return redirect{to: u}, nil
}

type redirect struct{ to *url.URL }

func (t redirect) RoundTrip(r *http.Request) (*http.Response, error) {
	switch strings.ToLower(r.URL.Hostname()) {
	case "github.com", "www.github.com", "api.github.com":
		r = r.Clone(r.Context())
		r.URL.Scheme, r.URL.Host, r.Host = t.to.Scheme, t.to.Host, t.to.Host
	}
	return http.DefaultTransport.RoundTrip(r)
}

// notify wakes the long polls. The caller holds s.mu.
func (s *Server) notify() {
	close(s.wake)
	s.wake = make(chan struct{})
}

// baseURL is the server URL as the client sees it.
func baseURL(r *http.Request) string {
	if r.TLS != nil {
		return "https://" + r.Host
	}
	return "http://" + r.Host
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(v)
}

// restError writes an error of the GitHub REST API.
func restError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{
		"message":           msg,
		"documentation_url": "https://docs.github.com/rest",
		"status":            strconv.Itoa(status),
	})
}

// actionsError writes an error of the Actions service. The scaleset client
// reads typeName to tell, for example, a missing runner from a busy one.
func actionsError(w http.ResponseWriter, status int, typeKey, msg string) {
	writeJSON(w, status, map[string]any{
		"$id":            "1",
		"innerException": nil,
		"message":        msg,
		"typeName":       "GitHub.DistributedTask.WebApi." + typeKey + ", GitHub.DistributedTask.WebApi",
		"typeKey":        typeKey,
		"errorCode":      0,
		"eventId":        3000,
	})
}

type tokenKind int

const (
	installToken tokenKind = iota + 1
	registrationToken
	adminToken
	queueToken
)

type token struct {
	kind    tokenKind
	install *installation // installToken
	scope   string        // registrationToken and adminToken: "acme" or "acme/api"
	session *session      // queueToken
	expires time.Time
}

const tokenTTL = time.Hour

// issue stores t and returns its value. The caller holds s.mu.
func (s *Server) issue(t *token) string {
	t.expires = time.Now().Add(tokenTTL)
	var v string
	switch t.kind {
	case installToken:
		v = "ghs_" + rand.Text()
	case adminToken:
		// The scaleset client reads the expiry from the admin token.
		v = unsignedJWT(map[string]any{"iss": "fakegh", "sub": t.scope, "exp": t.expires.Unix(), "jti": rand.Text()})
	default:
		v = rand.Text()
	}
	s.tokens[v] = t
	return v
}

// auth returns the token of kind in the Authorization header, whatever its
// scheme word ("Bearer", "token", "RemoteAuth"). The caller holds s.mu.
func (s *Server) auth(r *http.Request, kind tokenKind) *token {
	_, v, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	t := s.tokens[v]
	if t == nil || t.kind != kind || time.Now().After(t.expires) {
		return nil
	}
	return t
}

func unsignedJWT(claims map[string]any) string {
	enc := base64.RawURLEncoding
	c, _ := json.Marshal(claims)
	return enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc.EncodeToString(c) + "." + enc.EncodeToString([]byte(rand.Text()))
}

// checkAppJWT accepts any well-formed app JWT, as GitHub does apart from the
// signature: three parts, RS256, an issuer, and an expiry within the next
// 10 minutes. The error text is the GitHub message.
func checkAppJWT(r *http.Request) error {
	v, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(v, ".")
	if !ok || len(parts) != 3 {
		return errors.New("A JSON web token could not be decoded")
	}
	var header struct {
		Alg string `json:"alg"`
	}
	var claims struct {
		Iss any   `json:"iss"`
		Exp int64 `json:"exp"`
	}
	if decodeSegment(parts[0], &header) != nil || decodeSegment(parts[1], &claims) != nil || header.Alg != "RS256" {
		return errors.New("A JSON web token could not be decoded")
	}
	if claims.Iss == nil || claims.Iss == "" {
		return errors.New("'Issuer' claim ('iss') must be an Integer")
	}
	now := time.Now()
	exp := time.Unix(claims.Exp, 0)
	if !exp.After(now) {
		return errors.New("'Expiration time' claim ('exp') must be a numeric value representing the future time at which the assertion expires")
	}
	if exp.After(now.Add(10*time.Minute + 5*time.Second)) {
		return errors.New("'Expiration time' claim ('exp') is too far in the future")
	}
	return nil
}

func decodeSegment(seg string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
