package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/secret"
	"github.com/runplane/runplane/internal/store"
)

var (
	// enrichWait is how long a failed job waits for its run data (branch,
	// actor, run number) before its alert goes out without it.
	enrichWait = 2 * time.Minute
	// diskEvery is the shortest time between two disk.low alerts of a server.
	diskEvery = 6 * time.Hour
)

// diskMargin is how many percentage points the free disk space must rise
// above min_free_disk_percent before a new disk.low alert can go out.
const diskMargin = 5

// Dispatcher sends events to the notification channels that subscribe to
// them. Other packages give it events through Notify, and Run makes events
// from the bus: failed jobs, unreachable servers, and low disk space.
//
// Two sources can report the same thing, for example the fleet and the
// server.state event. The dispatcher sends one server.unreachable alert for
// each outage, one job.failed alert for each job, and at most one disk.low
// alert for each server in 6 hours.
type Dispatcher struct {
	Store *store.Store
	Box   *secret.Box // opens the channel URLs
	Log   *slog.Logger

	now func() time.Time // nil: time.Now
	due chan string      // failed jobs whose wait for run data ended
	wg  sync.WaitGroup

	mu       sync.Mutex
	minFree  float64              // min_free_disk_percent
	jobs     map[string]time.Time // failed jobs with a sent alert
	waiting  map[string]*time.Timer
	down     map[string]bool      // servers with a sent unreachable alert
	diskLow  map[string]bool      // servers below the free disk limit
	diskSent map[string]time.Time // last disk.low alert of each server
}

// Notify sends e to each enabled channel that subscribes to its type, and
// records each delivery. It drops an alert that an earlier event sent.
func (d *Dispatcher) Notify(ctx context.Context, e Event) error {
	if !d.admit(e) {
		return nil
	}
	list, err := d.Store.ListNotificationChannels(ctx)
	if err != nil {
		return err
	}
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, c := range list {
		if !c.Enabled || !slices.Contains(c.Events, string(e.Type)) {
			continue
		}
		wg.Go(func() {
			msg := ""
			if err := d.send(ctx, c, e); err != nil {
				msg = err.Error()
				mu.Lock()
				errs = append(errs, fmt.Errorf("channel %s: %w", c.Name, err))
				mu.Unlock()
			}
			if err := d.Store.RecordNotificationDelivery(context.WithoutCancel(ctx), c.ID, d.clock(), msg); err != nil {
				d.log().Warn("record notification delivery", "channel", c.Name, "err", err)
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// Test sends the test event to a channel: the api Notifier service.
func (d *Dispatcher) Test(ctx context.Context, base, kind, url string) error {
	return Send(ctx, Channel{Kind: Kind(kind), URL: url}, TestEvent(base))
}

func (d *Dispatcher) send(ctx context.Context, c store.NotificationChannel, e Event) error {
	url, err := d.Box.Open(c.URLEnc)
	if err != nil {
		return fmt.Errorf("open the channel URL: %w", err)
	}
	return Send(ctx, Channel{Kind: Kind(c.Kind), URL: string(url)}, e)
}

// admit reports if an alert goes out, and records it.
func (d *Dispatcher) admit(e Event) bool {
	id, _ := e.Data["server_id"].(string)
	now := d.clock()
	d.lock()
	defer d.mu.Unlock()
	switch e.Type {
	case ServerUnreachable:
		if d.down[id] {
			return false
		}
		d.down[id] = true
	case DiskLow:
		if last, ok := d.diskSent[id]; ok && now.Sub(last) < diskEvery {
			return false
		}
		d.diskSent[id] = now
	case JobFailed:
		job, _ := e.Data["job_id"].(string)
		if _, ok := d.jobs[job]; ok {
			return false
		}
		for id, at := range d.jobs {
			if now.Sub(at) > 24*time.Hour {
				delete(d.jobs, id)
			}
		}
		d.jobs[job] = now
	}
	return true
}

// Run makes alerts from the bus until ctx ends.
func (d *Dispatcher) Run(ctx context.Context, bus *events.Bus) {
	ch, cancel := bus.Subscribe(256)
	defer cancel()
	st, err := d.Store.GetSettings(ctx)
	if err != nil {
		d.log().Warn("read settings for notifications", "err", err)
	}
	d.lock()
	d.minFree = st.MinFreeDiskPercent
	d.due = make(chan string)
	d.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			d.lock()
			for _, t := range d.waiting {
				t.Stop()
			}
			d.mu.Unlock()
			d.wg.Wait()
			return
		case id := <-d.due:
			d.lock()
			delete(d.waiting, id)
			d.mu.Unlock()
			d.jobFailed(ctx, id)
		case e := <-ch:
			d.handle(ctx, e)
		}
	}
}

func (d *Dispatcher) handle(ctx context.Context, e events.Event) {
	switch e.Type {
	case events.SettingsChanged:
		if st, ok := as[store.Settings](e.Data); ok {
			d.lock()
			d.minFree = st.MinFreeDiskPercent
			d.mu.Unlock()
		}
	case events.JobState:
		if job, ok := as[store.Job](e.Data); ok && job.State == store.JobCompleted && job.Result == store.ResultFailed {
			d.jobDone(ctx, job)
		}
	case events.ServerState:
		v, ok := as[store.Server](e.Data)
		if !ok {
			return
		}
		switch v.Status {
		case store.ServerUnreachable:
			lastSeen := d.clock()
			if v.LastSeenAt != nil {
				lastSeen = *v.LastSeenAt
			}
			d.goNotify(ctx, func(dash string) Event { return ServerUnreachableEvent(dash, v.ID, v.Name, lastSeen) })
		case store.ServerOnline:
			d.lock()
			delete(d.down, v.ID)
			d.mu.Unlock()
		}
	case events.HostStats:
		if s, ok := as[store.ServerStat](e.Data); ok {
			d.hostStats(ctx, s)
		}
	}
}

// jobDone sends the alert of a failed job when its run data is there, or
// when the wait for the data ends.
func (d *Dispatcher) jobDone(ctx context.Context, job store.Job) {
	d.lock()
	defer d.mu.Unlock()
	if _, sent := d.jobs[job.ID]; sent {
		return
	}
	t, waits := d.waiting[job.ID]
	switch {
	case job.EnrichedAt != nil:
		if waits {
			t.Stop()
			delete(d.waiting, job.ID)
		}
		d.jobFailed(ctx, job.ID)
	case !waits:
		due := d.due
		d.waiting[job.ID] = time.AfterFunc(enrichWait, func() {
			select {
			case due <- job.ID:
			case <-ctx.Done():
			}
		})
	}
}

// jobFailed sends the alert of a failed job with the job data of the store.
func (d *Dispatcher) jobFailed(ctx context.Context, id string) {
	d.goNotify(ctx, func(dash string) Event {
		job, err := d.Store.GetJob(ctx, id)
		if err != nil {
			d.log().Warn("read the failed job", "job", id, "err", err)
		}
		var took time.Duration
		if job.StartedAt != nil && job.FinishedAt != nil {
			took = job.FinishedAt.Sub(*job.StartedAt)
		}
		return JobFailedEvent(dash, Job{
			ID: id, Repository: job.Repository, Workflow: job.WorkflowName, Name: job.JobName, Branch: job.HeadBranch,
			Actor: job.Actor, RunNumber: job.RunNumber, Duration: took, GitHubURL: job.HTMLURL,
		})
	})
}

// hostStats sends a disk.low alert when the free disk space of a server
// falls below min_free_disk_percent. The next alert waits until the space
// rises diskMargin points above the limit and falls again.
func (d *Dispatcher) hostStats(ctx context.Context, s store.ServerStat) {
	free := 100 - s.DiskPercent
	d.lock()
	minFree := d.minFree
	alert := free < minFree && !d.diskLow[s.ServerID]
	switch {
	case free < minFree:
		d.diskLow[s.ServerID] = true
	case free >= minFree+diskMargin:
		delete(d.diskLow, s.ServerID)
	}
	d.mu.Unlock()
	if !alert {
		return
	}
	d.goNotify(ctx, func(dash string) Event {
		name := s.ServerID
		if v, err := d.Store.GetServer(ctx, s.ServerID); err == nil {
			name = v.Name
		}
		return DiskLowEvent(dash, s.ServerID, name, free, int(minFree))
	})
}

// goNotify builds an event with the dashboard URL and sends it in the background.
func (d *Dispatcher) goNotify(ctx context.Context, build func(dashboard string) Event) {
	d.wg.Go(func() {
		st, err := d.Store.GetSettings(ctx)
		if err != nil {
			d.log().Warn("read settings for a notification", "err", err)
		}
		e := build(dashboard(st))
		if err := d.Notify(ctx, e); err != nil {
			d.log().Warn("send notification", "type", e.Type, "err", err)
		}
	})
}

// lock locks the alert state and makes its maps on first use.
func (d *Dispatcher) lock() {
	d.mu.Lock()
	if d.jobs == nil {
		d.jobs, d.diskSent = map[string]time.Time{}, map[string]time.Time{}
		d.waiting = map[string]*time.Timer{}
		d.down, d.diskLow = map[string]bool{}, map[string]bool{}
	}
}

func (d *Dispatcher) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

func (d *Dispatcher) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.New(slog.DiscardHandler)
}

// dashboard is the browser URL of runplane, or "" if runplane cannot know it.
func dashboard(st store.Settings) string {
	switch {
	case st.PublicURL != "":
		return st.PublicURL
	case st.Domain != "":
		return "https://" + st.Domain
	}
	return ""
}

// as returns v as a T, also when v is a *T.
func as[T any](v any) (T, bool) {
	switch x := v.(type) {
	case T:
		return x, true
	case *T:
		if x != nil {
			return *x, true
		}
	}
	var zero T
	return zero, false
}
