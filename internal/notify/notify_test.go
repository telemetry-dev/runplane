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

	"github.com/runplane/runplane/internal/store"
)

var at = time.Date(2026, 11, 4, 9, 30, 0, 0, time.UTC)

func failedJob() Event {
	started, finished := at.Add(-192500*time.Millisecond), at
	return JobFailedEvent("https://rp.example.com/", store.Job{
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

// receiver records the body of each POST and answers with the next status
// from statuses (the last one repeats).
type receiver struct {
	*httptest.Server
	calls  atomic.Int32
	bodies chan []byte
}

func newReceiver(t *testing.T, statuses ...int) *receiver {
	t.Helper()
	r := &receiver{bodies: make(chan []byte, 10)}
	r.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		n := int(r.calls.Add(1))
		if req.Method != http.MethodPost || req.Header.Get("Content-Type") != "application/json" {
			t.Errorf("got %s with Content-Type %q", req.Method, req.Header.Get("Content-Type"))
		}
		body, _ := io.ReadAll(req.Body)
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

func TestPayloads(t *testing.T) {
	const jobText = "acme/api · CI #42 · branch main · by octo · after 3m13s"
	tests := []struct {
		name string
		kind string
		ev   Event
		want string
	}{
		{"slack", Slack, failedJob(), `{
			"text": "*<https://rp.example.com/jobs/01jbq3x|Job failed: test (ubuntu)>*\n` + jobText + `",
			"unfurl_links": false, "unfurl_media": false}`},
		{"slack escapes markup", Slack,
			Event{Type: JobFailed, Title: "Job failed: <!channel> & co", Text: "x > y", URL: "https://rp.example.com/jobs/1"}, `{
			"text": "*<https://rp.example.com/jobs/1|Job failed: &lt;!channel&gt; &amp; co>*\nx &gt; y",
			"unfurl_links": false, "unfurl_media": false}`},
		{"discord", Discord, failedJob(), `{
			"embeds": [{"title": "Job failed: test (ubuntu)", "description": "` + jobText + `",
				"url": "https://rp.example.com/jobs/01jbq3x", "color": 14232864, "timestamp": "2026-11-04T09:30:00Z"}],
			"allowed_mentions": {"parse": []}}`},
		// Discord rejects an empty url and fields over its limits.
		{"discord truncates", Discord, BackupFailedEvent("", errors.New(strings.Repeat("é", 5000))), `{
			"embeds": [{"title": "Database backup failed", "description": "` + strings.Repeat("é", 4095) + `…",
				"color": 14232864, "timestamp": "2026-11-04T09:30:00Z"}],
			"allowed_mentions": {"parse": []}}`},
		{"webhook", Webhook, failedJob(), `{
			"event": "job.failed", "title": "Job failed: test (ubuntu)", "text": "` + jobText + `",
			"url": "https://rp.example.com/jobs/01jbq3x",
			"data": {"job_id": "01jbq3x", "repository": "acme/api", "workflow": "CI", "job_name": "test (ubuntu)",
				"branch": "main", "actor": "octo", "run_number": 42, "duration_ms": 192500,
				"github_url": "https://github.com/acme/api/actions/runs/1/job/2"},
			"at": "2026-11-04T09:30:00Z"}`},
		{"webhook without data", Webhook, TestEvent(""), `{
			"event": "test", "title": "Test notification from runplane",
			"text": "This channel receives runplane notifications.", "url": "", "data": {}, "at": "2026-11-04T09:30:00Z"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newReceiver(t, http.StatusOK)
			tt.ev.At = at
			if err := Send(context.Background(), tt.kind, r.URL+"/hook", tt.ev); err != nil {
				t.Fatal(err)
			}
			body := <-r.bodies
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

func TestSendRetries(t *testing.T) {
	prevTimeout, prevDelays := attemptTimeout, retryDelays
	attemptTimeout, retryDelays = 200*time.Millisecond, []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { attemptTimeout, retryDelays = prevTimeout, prevDelays })

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
			r := newReceiver(t, tt.statuses...)
			err := Send(context.Background(), Webhook, r.URL, TestEvent(""))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, want error %v", err, tt.wantErr)
			}
			if got := r.calls.Load(); got != tt.wantCalls {
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
		start := time.Now()
		if err := Send(context.Background(), Webhook, srv.URL, TestEvent("")); err == nil {
			t.Fatal("Send to a receiver that never answers succeeded")
		}
		// Three attempts of 200ms each.
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("Send took %v; the attempt timeout did not apply", elapsed)
		}
	})
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
		{JobFailedEvent("", store.Job{ID: "j1"}), ""},
	}
	for _, tt := range tests {
		if tt.ev.URL != tt.want {
			t.Errorf("%s: URL %q, want %q", tt.ev.Type, tt.ev.URL, tt.want)
		}
	}
}

func TestValidate(t *testing.T) {
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
	for _, tt := range tests {
		if err := validate(tt.kind, tt.url); (err != nil) != tt.wantErr {
			t.Errorf("%s %s: err = %v, want error %v", tt.kind, tt.url, err, tt.wantErr)
		}
	}
}
