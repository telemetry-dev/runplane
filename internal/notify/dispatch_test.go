package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/secret"
	"github.com/runplane/runplane/internal/store"
)

func TestDispatcherSendsToSubscribersAndRecordsDeliveries(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		mu.Unlock()
		if r.URL.Path == "/bad" {
			http.Error(w, "no such hook", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	prev := http.DefaultClient
	http.DefaultClient = srv.Client()
	defer func() { http.DefaultClient = prev }()

	st, err := store.Open(filepath.Join(t.TempDir(), "runplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	box, err := secret.NewBox(bytes.Repeat([]byte{3}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now()
	add := func(id, path string, enabled bool, events ...string) {
		t.Helper()
		err := st.CreateNotificationChannel(ctx, store.NotificationChannel{
			ID: id, Name: id, Kind: string(Webhook), URLEnc: box.Seal([]byte(srv.URL + path)),
			Events: events, Enabled: enabled, CreatedAt: now, UpdatedAt: now,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	add("ok", "/ok", true, string(JobFailed))
	add("bad", "/bad", true, string(DiskLow), string(JobFailed))
	add("off", "/off", false, string(JobFailed))
	add("other", "/other", true, string(DiskLow))

	d := &Dispatcher{Store: st, Box: box}
	err = d.Notify(ctx, failedJob())
	if err == nil || !strings.Contains(err.Error(), "channel bad") || strings.Contains(err.Error(), "channel ok") {
		t.Fatalf("Notify error = %v, want only the error of channel bad", err)
	}
	if hits["/ok"] != 1 || hits["/bad"] != 1 || hits["/off"] != 0 || hits["/other"] != 0 {
		t.Fatalf("requests by path = %v, want one to /ok and one to /bad", hits)
	}

	got, err := st.NotificationDeliveries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d := got["ok"]; d.SentAt == nil || d.Error != "" {
		t.Errorf("delivery of ok = %+v, want a time and no error", d)
	}
	if d := got["bad"]; d.SentAt == nil || !strings.Contains(d.Error, "404") {
		t.Errorf("delivery of bad = %+v, want a time and the 404 error", d)
	}
	for _, id := range []string{"off", "other"} {
		if d := got[id]; d.SentAt != nil {
			t.Errorf("delivery of %s = %+v, want none", id, d)
		}
	}
}

// hook is a webhook channel that records the events it gets.
type hook struct {
	mu   sync.Mutex
	got  []webhookBody
	done chan struct{} // gets a value for each request
}

type webhookBody struct {
	Event string         `json:"event"`
	Text  string         `json:"text"`
	Data  map[string]any `json:"data"`
}

func (h *hook) events() []webhookBody {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.got)
}

// newDispatcher returns a dispatcher with one webhook channel for types.
func newDispatcher(t *testing.T, types ...Type) (*Dispatcher, *hook) {
	t.Helper()
	h := &hook{done: make(chan struct{}, 10)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b webhookBody
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Errorf("webhook body: %v", err)
		}
		h.mu.Lock()
		h.got = append(h.got, b)
		h.mu.Unlock()
		select {
		case h.done <- struct{}{}:
		default:
		}
	}))
	t.Cleanup(srv.Close)
	st, err := store.Open(filepath.Join(t.TempDir(), "runplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, err := secret.NewBox(bytes.Repeat([]byte{7}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, typ := range types {
		names = append(names, string(typ))
	}
	err = st.CreateNotificationChannel(context.Background(), store.NotificationChannel{
		ID: "hook", Name: "hook", Kind: string(Webhook), URLEnc: box.Seal([]byte(srv.URL)),
		Events: names, Enabled: true, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Dispatcher{Store: st, Box: box}, h
}

func TestDispatcherSendsOneAlertForAFailedJob(t *testing.T) {
	d, h := newDispatcher(t, JobFailed)
	bus := runDispatcher(t, d)
	ctx := context.Background()

	jobs := map[string]store.Job{}
	for i, result := range []string{store.ResultFailed, store.ResultCancelled, store.ResultSucceeded} {
		j, err := d.Store.UpsertJob(ctx, store.JobMessage{RunnerRequestID: int64(i + 1), State: store.JobCompleted,
			Result: result, Repository: "acme/api", JobName: "test"})
		if err != nil {
			t.Fatal(err)
		}
		if result != store.ResultFailed {
			if err := d.Store.SetJobEnrichment(ctx, j.ID, store.JobEnrichment{HeadBranch: "main"}); err != nil {
				t.Fatal(err)
			}
			if j, err = d.Store.GetJob(ctx, j.ID); err != nil {
				t.Fatal(err)
			}
		}
		jobs[result] = j
	}
	failed := jobs[store.ResultFailed]
	// The completion comes before the run data: no alert yet.
	for range 15 {
		for _, j := range jobs {
			bus.Publish(events.Event{Type: events.JobState, Data: j})
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := h.events(); len(got) != 0 {
		t.Fatalf("alerts before the run data: %+v", got)
	}
	if err := d.Store.SetJobEnrichment(ctx, failed.ID, store.JobEnrichment{HeadBranch: "main", Actor: "octo"}); err != nil {
		t.Fatal(err)
	}
	enriched, err := d.Store.GetJob(ctx, failed.ID)
	if err != nil {
		t.Fatal(err)
	}
	publishUntilAlert(t, bus, h, events.Event{Type: events.JobState, Data: &enriched})
	for range 3 {
		bus.Publish(events.Event{Type: events.JobState, Data: enriched})
	}
	time.Sleep(100 * time.Millisecond)
	got := h.events()
	if len(got) != 1 {
		t.Fatalf("got %d alerts, want 1: %+v", len(got), got)
	}
	if got[0].Event != "job.failed" || got[0].Data["job_id"] != failed.ID || got[0].Data["branch"] != "main" || got[0].Data["actor"] != "octo" {
		t.Errorf("alert %+v, want job.failed of %s with its run data", got[0], failed.ID)
	}
}

func TestDispatcherSendsAFailedJobWithoutRunData(t *testing.T) {
	prev := enrichWait
	enrichWait = 50 * time.Millisecond
	t.Cleanup(func() { enrichWait = prev })
	d, h := newDispatcher(t, JobFailed)
	bus := runDispatcher(t, d)

	j, err := d.Store.UpsertJob(context.Background(), store.JobMessage{RunnerRequestID: 1, State: store.JobCompleted,
		Result: store.ResultFailed, Repository: "acme/api", JobName: "test"})
	if err != nil {
		t.Fatal(err)
	}
	publishUntilAlert(t, bus, h, events.Event{Type: events.JobState, Data: j})
	if got := h.events(); len(got) != 1 || got[0].Data["job_id"] != j.ID {
		t.Errorf("alerts %+v, want one for %s", got, j.ID)
	}
}

func TestDispatcherDiskLow(t *testing.T) {
	d, h := newDispatcher(t, DiskLow)
	ctx := context.Background()
	at := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	if err := d.Store.CreateServer(ctx, store.Server{ID: "s1", Name: "builder-1", Kind: store.ServerAgent,
		Status: store.ServerOnline, CreatedAt: at, UpdatedAt: at}); err != nil {
		t.Fatal(err)
	}
	now := at
	d.now = func() time.Time { return now }
	d.handle(ctx, events.Event{Type: events.SettingsChanged, Data: store.Settings{MinFreeDiskPercent: 15}})
	sample := func(after time.Duration, usedPercent float64) {
		now = at.Add(after)
		d.handle(ctx, events.Event{Type: events.HostStats, Data: store.ServerStat{ServerID: "s1", At: now, DiskPercent: usedPercent}})
		d.wg.Wait()
	}

	sample(0, 90) // 10% free: alert
	sample(time.Minute, 92)
	// The fleet reports the same drop.
	if err := d.Notify(ctx, DiskLowEvent("", "s1", "builder-1", 8, 15)); err != nil {
		t.Fatal(err)
	}
	sample(2*time.Minute, 82) // 18% free is not far enough above 15%
	sample(3*time.Minute, 90)
	sample(time.Hour, 70) // 30% free
	sample(2*time.Hour, 90)
	if got := h.events(); len(got) != 1 || got[0].Event != "disk.low" || got[0].Data["server_name"] != "builder-1" {
		t.Fatalf("alerts in the first 6 hours: %+v, want one disk.low of builder-1", got)
	}
	sample(7*time.Hour, 82)
	sample(7*time.Hour+time.Minute, 95)
	if n := len(h.events()); n != 1 {
		t.Fatalf("got %d alerts, want none after a rise to 18%% free", n)
	}
	sample(8*time.Hour, 70)
	sample(8*time.Hour+time.Minute, 95)
	if n := len(h.events()); n != 2 {
		t.Fatalf("got %d alerts, want a second one after 6 hours", n)
	}
}

func TestDispatcherServerUnreachableOncePerOutage(t *testing.T) {
	d, h := newDispatcher(t, ServerUnreachable)
	ctx := context.Background()
	down := store.Server{ID: "s1", Name: "builder-1", Status: store.ServerUnreachable}
	// The fleet and the server.state event report the same outage.
	if err := d.Notify(ctx, ServerUnreachableEvent("", "s1", "builder-1", time.Now())); err != nil {
		t.Fatal(err)
	}
	d.handle(ctx, events.Event{Type: events.ServerState, Data: down})
	d.wg.Wait()
	if n := len(h.events()); n != 1 {
		t.Fatalf("got %d alerts for one outage, want 1", n)
	}
	d.handle(ctx, events.Event{Type: events.ServerState, Data: store.Server{ID: "s1", Name: "builder-1", Status: store.ServerOnline}})
	d.handle(ctx, events.Event{Type: events.ServerState, Data: &down})
	d.wg.Wait()
	if got := h.events(); len(got) != 2 || got[1].Data["server_id"] != "s1" {
		t.Fatalf("alerts %+v, want a second one for the second outage", got)
	}
}

func runDispatcher(t *testing.T, d *Dispatcher) *events.Bus {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	bus := &events.Bus{}
	done := make(chan struct{})
	go func() { d.Run(ctx, bus); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return bus
}

// publishUntilAlert publishes the events again and again until the hook
// gets an alert, because Run subscribes to the bus in the background. Then
// it waits a little for a wrong second alert.
func publishUntilAlert(t *testing.T, bus *events.Bus, h *hook, list ...events.Event) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		for _, e := range list {
			bus.Publish(e)
		}
		select {
		case <-h.done:
			time.Sleep(200 * time.Millisecond)
			return
		case <-deadline:
			t.Fatal("no alert came")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
