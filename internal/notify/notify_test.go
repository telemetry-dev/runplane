package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/audit"
	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/field"
	"github.com/runplane/runplane/internal/secret"
	"github.com/runplane/runplane/internal/store"
)

var at = time.Date(2026, 11, 4, 9, 30, 0, 0, time.UTC)

func failedJob() Event {
	started, finished := at.Add(-192500*time.Millisecond), at
	return JobFailedEvent(store.Job{
		ID:           "01jbq3x",
		Repository:   "acme/api",
		WorkflowName: "CI",
		JobName:      "test (ubuntu)",
		HeadBranch:   "main",
		Actor:        "octo",
		RunNumber:    42,
		StartedAt:    &started,
		FinishedAt:   &finished,
		HTMLURL:      "https://github.com/acme/api/actions/runs/1/job/2",
	})
}

// env is a dispatcher with a real store and a receiver for its channels.
type env struct {
	st       *store.Store
	channels *Channels
	d        *Dispatcher
	bus      *events.Bus
	h        *hook
}

// newEnv makes the dispatcher. With tls, the receiver serves HTTPS, which
// Slack and Discord channels need.
func newEnv(t *testing.T, tls bool) *env {
	t.Helper()
	h := newHook(t, tls)
	st, err := store.Open(filepath.Join(t.TempDir(), "runplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, err := secret.NewBox(bytes.Repeat([]byte{7}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{st: st, channels: NewChannels(st, box, nil), bus: &events.Bus{}, h: h}
	e.d = New(Config{Store: st, Channels: e.channels, Bus: e.bus})
	return e
}

// run starts Run until the test ends. When Run ends, it waits for the
// sends, so no send outlives the receiver.
func (e *env) run(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.d.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

// subscribe adds the channel name of kind, with the URL path /name on the
// receiver, for types.
func (e *env) subscribe(t *testing.T, name, kind string, enabled bool, types ...Type) Channel {
	t.Helper()
	url := e.h.URL + "/" + name
	names := []string{}
	for _, typ := range types {
		names = append(names, string(typ))
	}
	c, err := e.channels.Create(context.Background(), audit.Actor{},
		ChannelChange{Name: &name, Kind: &kind, URL: &url, Events: &names, Enabled: &enabled})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (e *env) setDashboard(t *testing.T, publicURL, domain string) {
	t.Helper()
	if _, err := e.st.UpdateSettings(context.Background(), func(s *store.Settings) error {
		s.PublicURL, s.Domain = publicURL, domain
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// hook is a receiver that records each message. It answers 404 on the
// path /bad.
type hook struct {
	*httptest.Server
	mu   sync.Mutex
	got  []message
	sent chan struct{} // has a value after a request
}

// message is a message that the receiver got. The fields come from the
// body of a webhook channel.
type message struct {
	Path  string
	Body  []byte
	Event string         `json:"event"`
	URL   string         `json:"url"`
	Data  map[string]any `json:"data"`
}

func newHook(t *testing.T, tls bool) *hook {
	t.Helper()
	h := &hook{sent: make(chan struct{}, 1)}
	h.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("got %s with Content-Type %q", r.Method, r.Header.Get("Content-Type"))
		}
		m := message{Path: r.URL.Path}
		m.Body, _ = io.ReadAll(r.Body)
		json.Unmarshal(m.Body, &m)
		h.mu.Lock()
		h.got = append(h.got, m)
		h.mu.Unlock()
		select {
		case h.sent <- struct{}{}:
		default:
		}
		if r.URL.Path == "/bad" {
			http.Error(w, "no such hook", http.StatusNotFound)
		}
	}))
	if !tls {
		h.Start()
		t.Cleanup(h.Close)
		return h
	}
	h.StartTLS()
	t.Cleanup(h.Close)
	// The channels send with http.DefaultClient; trust the test certificate there.
	prev := http.DefaultClient
	http.DefaultClient = h.Client()
	t.Cleanup(func() { http.DefaultClient = prev })
	return h
}

func (h *hook) messages() []message {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.got)
}

// waitFor waits until the messages pass ok, and returns them.
func (h *hook) waitFor(t *testing.T, what string, ok func([]message) bool) []message {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if got := h.messages(); ok(got) {
			return got
		}
		select {
		case <-h.sent:
		case <-deadline:
			t.Fatalf("no %s in 5 seconds; got %+v", what, h.messages())
		}
	}
}

func TestPayloads(t *testing.T) {
	const jobText = "acme/api · CI #42 · branch main · by octo · after 3m13s"
	const dashboard = "https://rp.example.com/"
	tests := []struct {
		name      string
		kind      string
		ev        Event
		dashboard string
		want      string
	}{
		{"slack", Slack, failedJob(), dashboard, `{
			"text": "*<https://rp.example.com/jobs/01jbq3x|Job failed: test (ubuntu)>*\n` + jobText + `",
			"unfurl_links": false, "unfurl_media": false}`},
		{"slack escapes markup", Slack,
			Event{Type: JobFailed, Title: "Job failed: <!channel> & co", Text: "x > y", Path: "/jobs/1"}, dashboard, `{
			"text": "*<https://rp.example.com/jobs/1|Job failed: &lt;!channel&gt; &amp; co>*\nx &gt; y",
			"unfurl_links": false, "unfurl_media": false}`},
		{"discord", Discord, failedJob(), dashboard, `{
			"embeds": [{"title": "Job failed: test (ubuntu)", "description": "` + jobText + `",
				"url": "https://rp.example.com/jobs/01jbq3x", "color": 14232864, "timestamp": "2026-11-04T09:30:00Z"}],
			"allowed_mentions": {"parse": []}}`},
		// Discord rejects an empty url and fields over its limits.
		{"discord truncates", Discord, BackupFailedEvent(strings.Repeat("é", 5000)), "", `{
			"embeds": [{"title": "Database backup failed", "description": "` + strings.Repeat("é", 4095) + `…",
				"color": 14232864, "timestamp": "2026-11-04T09:30:00Z"}],
			"allowed_mentions": {"parse": []}}`},
		{"webhook", Webhook, failedJob(), dashboard, `{
			"event": "job.failed", "title": "Job failed: test (ubuntu)", "text": "` + jobText + `",
			"url": "https://rp.example.com/jobs/01jbq3x",
			"data": {"job_id": "01jbq3x", "repository": "acme/api", "workflow": "CI", "job_name": "test (ubuntu)",
				"branch": "main", "actor": "octo", "run_number": 42, "duration_ms": 192500,
				"github_url": "https://github.com/acme/api/actions/runs/1/job/2"},
			"at": "2026-11-04T09:30:00Z"}`},
		{"webhook without data", Webhook, Event{Type: DiskLow, Title: "Disk almost full", Text: "Free some space."}, "", `{
			"event": "disk.low", "title": "Disk almost full", "text": "Free some space.", "url": "", "data": {},
			"at": "2026-11-04T09:30:00Z"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, true)
			e.setDashboard(t, tt.dashboard, "")
			e.subscribe(t, "hook", tt.kind, true, tt.ev.Type)
			e.run(t)
			tt.ev.At = at
			e.d.Notify(tt.ev)
			body := e.h.waitFor(t, "message", func(m []message) bool { return len(m) > 0 })[0].Body
			var got, want any
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("body is not JSON: %s", body)
			}
			if err := json.Unmarshal([]byte(tt.want), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("payload:\n%s", body)
			}
		})
	}
}

// Each alert links to its page on the dashboard.
func TestEventLinks(t *testing.T) {
	e := newEnv(t, false)
	e.setDashboard(t, "https://rp.example.com/", "")
	e.subscribe(t, "hook", Webhook, true, Types...)
	e.run(t)
	for _, ev := range []Event{
		ServerUnreachableEvent("srv1", "edge-1", at),
		DiskLowEvent("srv2", "edge-2", 9.6, 15),
		RunnerDeprecationEvent("2.329.0", time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)),
		UpdateAvailableEvent("v0.4.2", "v0.5.0", ""),
		BackupFailedEvent("disk full"),
	} {
		e.d.Notify(ev)
	}
	got := map[string]string{}
	for _, m := range e.h.waitFor(t, "5 alerts", func(m []message) bool { return len(m) == 5 }) {
		got[m.Event] = m.URL
	}
	want := map[string]string{
		"server.unreachable": "https://rp.example.com/servers/srv1",
		"disk.low":           "https://rp.example.com/servers/srv2",
		"runner.deprecation": "https://rp.example.com/settings/runners",
		"update.available":   "https://rp.example.com/settings/general",
		"backup.failed":      "https://rp.example.com/settings/general",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("links %v, want %v", got, want)
	}
}

func TestSendRetries(t *testing.T) {
	prevTimeout, prevDelays := attemptTimeout, retryDelays
	attemptTimeout, retryDelays = 200*time.Millisecond, []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { attemptTimeout, retryDelays = prevTimeout, prevDelays })
	channels := NewChannels(nil, nil, nil)

	tests := []struct {
		name      string
		statuses  []int // the last one repeats
		wantErr   bool
		wantCalls int32
	}{
		{"recovers after two 5xx", []int{500, 503, 200}, false, 3},
		{"stops after two retries", []int{502}, true, 3},
		{"retries rate limit", []int{429, 204}, false, 2},
		{"no retry on 4xx", []int{404}, true, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := int(calls.Add(1))
				w.WriteHeader(tt.statuses[min(n, len(tt.statuses))-1])
			}))
			defer srv.Close()
			err := channels.TestURL(context.Background(), Webhook, srv.URL, "")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, want error %v", err, tt.wantErr)
			}
			if got := calls.Load(); got != tt.wantCalls {
				t.Fatalf("%d requests, want %d", got, tt.wantCalls)
			}
		})
	}

	t.Run("attempt timeout", func(t *testing.T) {
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}))
		defer srv.Close()
		defer close(release)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		start := time.Now()
		if err := channels.TestURL(ctx, Webhook, srv.URL, ""); err == nil {
			t.Fatal("a send to a receiver that never answers succeeded")
		}
		// Three attempts of 200ms each.
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("Send took %v; the attempt timeout did not apply", elapsed)
		}
	})
}

func TestChannelTargets(t *testing.T) {
	tests := []struct {
		kind, url string
		wantErr   bool
	}{
		{Slack, "https://hooks.slack.com/services/T/B/x", false},
		{Webhook, "http://10.0.0.5:9000/hook", false},
		{Slack, "http://hooks.slack.com/services/T/B/x", true},
		{Discord, "discord.com/api/webhooks/1/x", true},
		{"teams", "https://example.com", true},
		{Webhook, "ftp://example.com", true},
	}
	e := newEnv(t, false)
	for _, tt := range tests {
		name := tt.kind + " " + tt.url
		_, err := e.channels.Create(context.Background(), audit.Actor{}, ChannelChange{Name: &name, Kind: &tt.kind, URL: &tt.url})
		var p field.Problems
		if (err != nil) != tt.wantErr || err != nil && !errors.As(err, &p) {
			t.Errorf("%s %s: err = %v, want field problems %v", tt.kind, tt.url, err, tt.wantErr)
		}
	}
}
