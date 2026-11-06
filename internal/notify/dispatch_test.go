package notify

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/store"
)

// The dispatcher sends each alert in the background. A test that wants no
// alert for some events publishes them, then flushes: it publishes an event
// of the same type with an alert, and waits for that alert. The bus keeps
// the order of one type, so the dispatcher handled the events before it.
// Before a test counts the alerts, it flushes settleFlushes times, so that
// a wrong alert of an earlier event has the time of several sends to come.
const settleFlushes = 3

// with returns the messages whose data has key with value.
func with(list []message, key string, value any) []message {
	var out []message
	for _, m := range list {
		if m.Data[key] == value {
			out = append(out, m)
		}
	}
	return out
}

func TestDispatcherSendsToSubscribersAndRecordsDeliveries(t *testing.T) {
	e := newEnv(t, false)
	e.subscribe(t, "ok", Webhook, true, JobFailed)
	e.subscribe(t, "bad", Webhook, true, DiskLow, JobFailed)
	e.subscribe(t, "off", Webhook, false, JobFailed)
	e.subscribe(t, "other", Webhook, true, DiskLow)
	e.run(t)
	e.d.Notify(failedJob())

	// The delivery of a channel is recorded after its send.
	var last map[string]store.NotificationDelivery
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		list, err := e.channels.List(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		last = map[string]store.NotificationDelivery{}
		for _, c := range list {
			last[c.Name] = c.Last
		}
		if last["ok"].SentAt != nil && last["bad"].SentAt != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("deliveries %+v, want one of ok and one of bad", last)
		}
	}
	if d := last["ok"]; d.Error != "" {
		t.Errorf("delivery of ok = %+v, want no error", d)
	}
	if d := last["bad"]; !strings.Contains(d.Error, "404") {
		t.Errorf("delivery of bad = %+v, want the 404 error", d)
	}
	for _, name := range []string{"off", "other"} {
		if d := last[name]; d.SentAt != nil {
			t.Errorf("delivery of %s = %+v, want none", name, d)
		}
	}
	var paths []string
	for _, m := range e.h.messages() {
		paths = append(paths, m.Path)
	}
	slices.Sort(paths)
	if !slices.Equal(paths, []string{"/bad", "/ok"}) {
		t.Errorf("requests to %q, want one to /bad and one to /ok", paths)
	}
}

func TestDispatcherSendsOneAlertForAFailedJob(t *testing.T) {
	e := newEnv(t, false)
	e.subscribe(t, "hook", Webhook, true, JobFailed)
	e.run(t)
	ctx := context.Background()
	n := int64(0)
	job := func(result string, run *store.JobEnrichment) store.Job {
		t.Helper()
		n++
		j, err := e.st.UpsertJob(ctx, store.JobMessage{RunnerRequestID: n, State: store.JobCompleted,
			Result: result, Repository: "acme/api", JobName: "test"}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if run != nil {
			if err := e.st.SetJobEnrichment(ctx, j.ID, *run, time.Now()); err != nil {
				t.Fatal(err)
			}
			if j, err = e.st.GetJob(ctx, j.ID); err != nil {
				t.Fatal(err)
			}
		}
		return j
	}
	var flushed []string // the jobs of the flushes
	settle := func() {
		t.Helper()
		for range settleFlushes {
			j := job(store.ResultFailed, &store.JobEnrichment{HeadBranch: "main"})
			flushed = append(flushed, j.ID)
			e.bus.Publish(events.JobState(j))
			e.h.waitFor(t, "the alert of "+j.ID, func(m []message) bool { return len(with(m, "job_id", j.ID)) > 0 })
		}
	}

	// The completion comes before the run data: no alert yet.
	failed := job(store.ResultFailed, nil)
	for _, j := range []store.Job{
		failed,
		job(store.ResultCancelled, &store.JobEnrichment{HeadBranch: "main"}),
		job(store.ResultSucceeded, &store.JobEnrichment{HeadBranch: "main"}),
	} {
		e.bus.Publish(events.JobState(j))
	}
	settle()
	if got := e.h.messages(); len(got) != len(flushed) {
		t.Fatalf("alerts %+v, want only the alerts of the flushes", got)
	}

	// The run data comes, and later changes of the job send more events.
	if err := e.st.SetJobEnrichment(ctx, failed.ID, store.JobEnrichment{HeadBranch: "main", Actor: "octo"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	enriched, err := e.st.GetJob(ctx, failed.ID)
	if err != nil {
		t.Fatal(err)
	}
	for range 4 {
		e.bus.Publish(events.JobState(enriched))
	}
	e.h.waitFor(t, "the alert of the failed job", func(m []message) bool { return len(with(m, "job_id", failed.ID)) > 0 })
	settle()
	got := with(e.h.messages(), "job_id", failed.ID)
	if len(got) != 1 || got[0].Event != "job.failed" || got[0].Data["branch"] != "main" || got[0].Data["actor"] != "octo" {
		t.Errorf("alerts %+v, want one job.failed of %s with its run data", got, failed.ID)
	}
}

func TestDispatcherSendsAFailedJobWithoutRunData(t *testing.T) {
	prev := enrichWait
	enrichWait = 50 * time.Millisecond
	t.Cleanup(func() { enrichWait = prev })
	e := newEnv(t, false)
	e.subscribe(t, "hook", Webhook, true, JobFailed)
	e.run(t)

	j, err := e.st.UpsertJob(context.Background(), store.JobMessage{RunnerRequestID: 1, State: store.JobCompleted,
		Result: store.ResultFailed, Repository: "acme/api", JobName: "test"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e.bus.Publish(events.JobState(j))
	if got := e.h.waitFor(t, "an alert", func(m []message) bool { return len(m) > 0 }); got[0].Data["job_id"] != j.ID {
		t.Errorf("alert %+v, want the alert of %s", got[0], j.ID)
	}
}

// Each backup row goes out on the bus. Only a failed backup sends an
// alert, with the error of the row. The dispatcher also gets the events
// that come before Run starts.
func TestDispatcherSendsAFailedBackup(t *testing.T) {
	e := newEnv(t, false)
	e.subscribe(t, "hook", Webhook, true, BackupFailed)
	now := time.Now()
	e.bus.Publish(events.BackupState{ID: "b1", CreatedAt: now, Status: store.BackupDone})
	e.bus.Publish(events.BackupState{ID: "b1", CreatedAt: now, Status: store.BackupUploaded})
	const reason = "upload to S3: access denied"
	e.bus.Publish(events.BackupState{ID: "b2", CreatedAt: now, Status: store.BackupFailed, Error: reason})
	e.run(t)
	e.h.waitFor(t, "the alert of b2", func(m []message) bool { return len(with(m, "error", reason)) > 0 })
	for i := range settleFlushes {
		flush := fmt.Sprint("flush ", i)
		e.bus.Publish(events.BackupState{ID: flush, CreatedAt: now, Status: store.BackupFailed, Error: flush})
		e.h.waitFor(t, "the alert of "+flush, func(m []message) bool { return len(with(m, "error", flush)) > 0 })
	}
	got := e.h.messages()
	if b2 := with(got, "error", reason); len(got) != 1+settleFlushes || len(b2) != 1 || b2[0].Event != "backup.failed" {
		t.Errorf("alerts %+v, want one backup.failed with the error of b2, and the alerts of the flushes", got)
	}
}

func TestDispatcherDiskLow(t *testing.T) {
	e := newEnv(t, false)
	e.subscribe(t, "hook", Webhook, true, DiskLow)
	ctx := context.Background()
	start := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	if err := e.st.CreateServer(ctx, store.Server{ID: "s1", Name: "builder-1", Kind: store.ServerAgent,
		Status: store.ServerOnline, CreatedAt: start, UpdatedAt: start}); err != nil {
		t.Fatal(err)
	}
	// With a domain and no public URL, the links go to the domain. Run
	// reads the limit from the settings.
	if _, err := e.st.UpdateSettings(ctx, func(s *store.Settings) error {
		s.Domain, s.MinFreeDiskPercent = "ci.example.com", 20
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The dispatcher has no clock option, so the test sets its clock.
	var now atomic.Int64
	now.Store(start.UnixNano())
	e.d.now = func() time.Time { return time.Unix(0, now.Load()) }
	e.run(t)

	flushes := 0
	flush := func() {
		t.Helper()
		flushes++
		id := fmt.Sprint("flush-", flushes)
		e.bus.Publish(events.HostStats{ServerID: id, DiskPercent: 100})
		e.h.waitFor(t, "the alert of "+id, func(m []message) bool { return len(with(m, "server_id", id)) > 0 })
	}
	// sample gives the dispatcher a sample at a time, and waits until it
	// handled it, before the clock moves.
	sample := func(after time.Duration, usedPercent float64) {
		t.Helper()
		now.Store(start.Add(after).UnixNano())
		e.bus.Publish(events.HostStats{ServerID: "s1", DiskPercent: usedPercent})
		flush()
	}
	alerts := func(n int) []message {
		t.Helper()
		e.h.waitFor(t, fmt.Sprint(n, " alerts of s1"), func(m []message) bool { return len(with(m, "server_id", "s1")) >= n })
		for range settleFlushes {
			flush()
		}
		return with(e.h.messages(), "server_id", "s1")
	}

	// The samples keep 16% free or more, so the default limit of 15% sends
	// no alert.
	sample(0, 83) // 17% free: alert
	sample(time.Minute, 84)
	sample(2*time.Minute, 77) // 23% free is not far enough above 20%
	sample(3*time.Minute, 83)
	sample(time.Hour, 65) // 35% free
	sample(2*time.Hour, 83)
	if got := alerts(1); len(got) != 1 || got[0].Event != "disk.low" || got[0].Data["server_name"] != "builder-1" ||
		got[0].URL != "https://ci.example.com/servers/s1" {
		t.Fatalf("alerts in the first 6 hours: %+v, want one disk.low of builder-1 with a link to the domain", got)
	}
	sample(7*time.Hour, 77)
	sample(7*time.Hour+time.Minute, 84)
	if n := len(alerts(1)); n != 1 {
		t.Fatalf("got %d alerts, want none after a rise to 23%% free", n)
	}
	sample(8*time.Hour, 65)
	sample(8*time.Hour+time.Minute, 84)
	if n := len(alerts(2)); n != 2 {
		t.Fatalf("got %d alerts, want a second one after 6 hours", n)
	}
}

func TestDispatcherServerUnreachableOncePerOutage(t *testing.T) {
	e := newEnv(t, false)
	e.subscribe(t, "hook", Webhook, true, ServerUnreachable)
	e.run(t)
	flushes := 0
	flush := func() {
		t.Helper()
		flushes++
		id := fmt.Sprint("flush-", flushes)
		e.bus.Publish(events.ServerState{ID: id, Name: id, Status: store.ServerUnreachable})
		e.h.waitFor(t, "the alert of "+id, func(m []message) bool { return len(with(m, "server_id", id)) > 0 })
	}
	alerts := func(n int) []message {
		t.Helper()
		e.h.waitFor(t, fmt.Sprint(n, " alerts of s1"), func(m []message) bool { return len(with(m, "server_id", "s1")) >= n })
		for range settleFlushes {
			flush()
		}
		return with(e.h.messages(), "server_id", "s1")
	}

	// Each change of the row during the outage sends a server.state event.
	down := events.ServerState{ID: "s1", Name: "builder-1", Status: store.ServerUnreachable}
	e.bus.Publish(down)
	e.bus.Publish(down)
	if n := len(alerts(1)); n != 1 {
		t.Fatalf("got %d alerts for one outage, want 1", n)
	}
	e.bus.Publish(events.ServerState{ID: "s1", Name: "builder-1", Status: store.ServerOnline})
	e.bus.Publish(down)
	if n := len(alerts(2)); n != 2 {
		t.Fatalf("got %d alerts, want a second one for the second outage", n)
	}
}
