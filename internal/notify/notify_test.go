package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var at = time.Date(2026, 11, 4, 9, 30, 0, 0, time.UTC)

func failedJob() Event {
	ev := JobFailedEvent("https://rp.example.com/", Job{
		ID:         "01jbq3x",
		Repository: "acme/api",
		Workflow:   "CI",
		Name:       "test (ubuntu)",
		Branch:     "main",
		Actor:      "octo",
		RunNumber:  42,
		Duration:   192500 * time.Millisecond,
		GitHubURL:  "https://github.com/acme/api/actions/runs/1/job/2",
	})
	ev.At = at
	return ev
}

// receiver records the JSON body of each request and answers with the
// next status from statuses (the last one repeats).
type receiver struct {
	*httptest.Server
	calls  atomic.Int32
	bodies chan map[string]any
}

func newReceiver(t *testing.T, statuses ...int) *receiver {
	t.Helper()
	r := &receiver{bodies: make(chan map[string]any, 10)}
	r.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		n := int(r.calls.Add(1))
		if req.Method != http.MethodPost || req.Header.Get("Content-Type") != "application/json" {
			t.Errorf("got %s with Content-Type %q", req.Method, req.Header.Get("Content-Type"))
		}
		var body map[string]any
		data, _ := io.ReadAll(req.Body)
		if err := json.Unmarshal(data, &body); err != nil {
			t.Errorf("body is not JSON: %s", data)
		}
		r.bodies <- body
		w.WriteHeader(statuses[min(n, len(statuses))-1])
	}))
	t.Cleanup(r.Close)
	// Send uses http.DefaultClient; trust the test certificate there.
	prev := http.DefaultClient
	http.DefaultClient = r.Client()
	t.Cleanup(func() { http.DefaultClient = prev })
	return r
}

func shortRetries(t *testing.T) {
	prevTimeout, prevDelays := attemptTimeout, retryDelays
	attemptTimeout, retryDelays = 200*time.Millisecond, []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { attemptTimeout, retryDelays = prevTimeout, prevDelays })
}

func TestPayloads(t *testing.T) {
	tests := []struct {
		kind Kind
		want map[string]any
	}{
		{Slack, map[string]any{
			"text":         "*<https://rp.example.com/jobs/01jbq3x|Job failed: test (ubuntu)>*\nacme/api · CI #42 · branch main · by octo · after 3m13s",
			"unfurl_links": false,
			"unfurl_media": false,
		}},
		{Discord, map[string]any{
			"embeds": []any{map[string]any{
				"title":       "Job failed: test (ubuntu)",
				"description": "acme/api · CI #42 · branch main · by octo · after 3m13s",
				"url":         "https://rp.example.com/jobs/01jbq3x",
				"color":       float64(0xd92d20),
				"timestamp":   "2026-11-04T09:30:00Z",
			}},
			"allowed_mentions": map[string]any{"parse": []any{}},
		}},
		{Webhook, map[string]any{
			"event": "job.failed",
			"title": "Job failed: test (ubuntu)",
			"text":  "acme/api · CI #42 · branch main · by octo · after 3m13s",
			"url":   "https://rp.example.com/jobs/01jbq3x",
			"data": map[string]any{
				"job_id":      "01jbq3x",
				"repository":  "acme/api",
				"workflow":    "CI",
				"job_name":    "test (ubuntu)",
				"branch":      "main",
				"actor":       "octo",
				"run_number":  float64(42),
				"duration_ms": float64(192500),
				"github_url":  "https://github.com/acme/api/actions/runs/1/job/2",
			},
			"at": "2026-11-04T09:30:00Z",
		}},
	}
	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			r := newReceiver(t, http.StatusOK)
			if err := Send(context.Background(), Channel{Kind: tt.kind, URL: r.URL + "/hook"}, failedJob()); err != nil {
				t.Fatal(err)
			}
			if got := <-r.bodies; !reflect.DeepEqual(got, tt.want) {
				gotJSON, _ := json.MarshalIndent(got, "", "  ")
				t.Fatalf("payload:\n%s", gotJSON)
			}
		})
	}
}

func TestSlackEscapesMarkup(t *testing.T) {
	r := newReceiver(t, http.StatusOK)
	ev := Event{Type: JobFailed, Title: "Job failed: <!channel> & co", Text: "x > y", URL: "https://rp.example.com/jobs/1"}
	if err := Send(context.Background(), Channel{Kind: Slack, URL: r.URL}, ev); err != nil {
		t.Fatal(err)
	}
	want := "*<https://rp.example.com/jobs/1|Job failed: &lt;!channel&gt; &amp; co>*\nx &gt; y"
	if got := (<-r.bodies)["text"]; got != want {
		t.Fatalf("text %q, want %q", got, want)
	}
}

func TestDiscordTruncatesLongFields(t *testing.T) {
	r := newReceiver(t, http.StatusNoContent)
	ev := BackupFailedEvent("", errors.New(strings.Repeat("é", 5000)))
	if err := Send(context.Background(), Channel{Kind: Discord, URL: r.URL}, ev); err != nil {
		t.Fatal(err)
	}
	embed := (<-r.bodies)["embeds"].([]any)[0].(map[string]any)
	if n := len([]rune(embed["description"].(string))); n != 4096 {
		t.Fatalf("description has %d characters, want 4096", n)
	}
	if _, ok := embed["url"]; ok {
		t.Fatal("embed has a url without a dashboard URL; Discord rejects an empty url")
	}
}

func TestSendRetries(t *testing.T) {
	tests := []struct {
		name      string
		statuses  []int
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
			shortRetries(t)
			r := newReceiver(t, tt.statuses...)
			err := Send(context.Background(), Channel{Kind: Webhook, URL: r.URL}, TestEvent(""))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, want error %v", err, tt.wantErr)
			}
			if got := r.calls.Load(); got != tt.wantCalls {
				t.Fatalf("%d requests, want %d", got, tt.wantCalls)
			}
		})
	}
}

func TestSendTimeout(t *testing.T) {
	shortRetries(t)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	start := time.Now()
	err := Send(context.Background(), Channel{Kind: Webhook, URL: srv.URL}, TestEvent(""))
	if err == nil {
		t.Fatal("Send to a receiver that never answers succeeded")
	}
	// Three attempts of 200ms each.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Send took %v; the attempt timeout did not apply", elapsed)
	}
}

func TestEventLinks(t *testing.T) {
	end := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		ev   Event
		want string
	}{
		{ServerUnreachableEvent("https://rp.example.com/", "srv1", "edge-1", at), "https://rp.example.com/servers/srv1"},
		{DiskLowEvent("https://rp.example.com", "srv1", "edge-1", 9.6, 15), "https://rp.example.com/servers/srv1"},
		{RunnerDeprecationEvent("https://rp.example.com", "2.329.0", end), "https://rp.example.com/settings/runners"},
		{UpdateAvailableEvent("https://rp.example.com", "v0.4.2", "v0.5.0", ""), "https://rp.example.com/settings/general"},
		{BackupFailedEvent("https://rp.example.com", errors.New("disk full")), "https://rp.example.com/settings/general"},
		{JobFailedEvent("", Job{ID: "j1"}), ""},
	}
	for _, tt := range tests {
		if tt.ev.URL != tt.want {
			t.Errorf("%s: URL %q, want %q", tt.ev.Type, tt.ev.URL, tt.want)
		}
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		ch      Channel
		wantErr bool
	}{
		{Channel{Slack, "https://hooks.slack.com/services/T/B/x"}, false},
		{Channel{Webhook, "http://10.0.0.5:9000/hook"}, false},
		{Channel{Slack, "http://hooks.slack.com/services/T/B/x"}, true},
		{Channel{Discord, "discord.com/api/webhooks/1/x"}, true},
		{Channel{"teams", "https://example.com"}, true},
		{Channel{Webhook, "ftp://example.com"}, true},
	}
	for _, tt := range tests {
		if err := tt.ch.Validate(); (err != nil) != tt.wantErr {
			t.Errorf("%v: err = %v, want error %v", tt.ch, err, tt.wantErr)
		}
	}
}
