package notify

import (
	"cmp"
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

// enrichWait is how long a failed job waits for its run data (branch,
// actor, run number) before its alert goes out without it.
var enrichWait = 2 * time.Minute

const (
	// diskEvery is the shortest time between two disk.low alerts of a server.
	diskEvery = 6 * time.Hour
	// diskMargin is how many percentage points the free disk space must rise
	// above min_free_disk_percent before a new disk.low alert can go out.
	diskMargin = 5
)

// Dispatcher sends events to the notification channels that subscribe to
// them. Other packages give it events through Notify. Run makes the alerts
// of the bus: one job.failed alert for each failed job, one
// server.unreachable alert for each outage of a server, and at most one
// disk.low alert for each server in 6 hours.
type Dispatcher struct {
	st  *store.Store
	box *secret.Box // opens the channel URLs
	log *slog.Logger
	now func() time.Time

	settings <-chan events.SettingsChanged
	jobs     <-chan events.JobState
	servers  <-chan events.ServerState
	stats    <-chan events.HostStats
	stop     context.CancelFunc // ends the subscriptions

	due chan string // failed jobs whose wait for run data ended
	wg  sync.WaitGroup

	mu       sync.Mutex
	minFree  float64              // min_free_disk_percent
	sent     map[string]time.Time // failed jobs with a sent alert, by job ID
	waiting  map[string]*time.Timer
	down     map[string]bool      // servers with a sent unreachable alert
	diskLow  map[string]bool      // servers below the free disk limit
	diskSent map[string]time.Time // last disk.low alert of each server
}

type Config struct {
	Store *store.Store
	Box   *secret.Box
	Bus   *events.Bus
	Log   *slog.Logger
}

// New makes a dispatcher. It takes its events from the bus at once, also the
// events before Run starts.
func New(c Config) *Dispatcher {
	ctx, stop := context.WithCancel(context.Background())
	return &Dispatcher{
		st:       c.Store,
		box:      c.Box,
		log:      cmp.Or(c.Log, slog.New(slog.DiscardHandler)),
		now:      time.Now,
		settings: events.Subscribe[events.SettingsChanged](ctx, c.Bus),
		jobs:     events.Subscribe[events.JobState](ctx, c.Bus),
		servers:  events.Subscribe[events.ServerState](ctx, c.Bus),
		stats:    events.Subscribe[events.HostStats](ctx, c.Bus),
		stop:     stop,
		due:      make(chan string),
		sent:     map[string]time.Time{},
		waiting:  map[string]*time.Timer{},
		down:     map[string]bool{},
		diskLow:  map[string]bool{},
		diskSent: map[string]time.Time{},
	}
}

// Notify sends e to each enabled channel that subscribes to its type, and
// records each delivery.
func (d *Dispatcher) Notify(ctx context.Context, e Event) error {
	list, err := d.st.ListNotificationChannels(ctx)
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	errs := make([]error, len(list))
	for i, c := range list {
		if !c.Enabled || !slices.Contains(c.Events, string(e.Type)) {
			continue
		}
		wg.Go(func() {
			msg := ""
			if err := d.send(ctx, c, e); err != nil {
				msg = err.Error()
				errs[i] = fmt.Errorf("channel %s: %w", c.Name, err)
			}
			if err := d.st.RecordNotificationDelivery(context.WithoutCancel(ctx), c.ID, d.now(), msg); err != nil {
				d.log.Warn("record notification delivery", "channel", c.Name, "err", err)
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// Test sends the test event to a channel: the api Notifier service.
func (d *Dispatcher) Test(ctx context.Context, base, kind, url string) error {
	return Send(ctx, kind, url, TestEvent(base))
}

func (d *Dispatcher) send(ctx context.Context, c store.NotificationChannel, e Event) error {
	url, err := d.box.Open(c.URLEnc)
	if err != nil {
		return fmt.Errorf("open the channel URL: %w", err)
	}
	return Send(ctx, c.Kind, string(url), e)
}

// Run makes alerts from the bus until ctx ends.
func (d *Dispatcher) Run(ctx context.Context) {
	defer d.stop()
	st, err := d.st.GetSettings(ctx)
	if err != nil {
		d.log.Warn("read settings for notifications", "err", err)
	}
	d.settingsChanged(st)
	for {
		select {
		case <-ctx.Done():
			d.mu.Lock()
			for _, t := range d.waiting {
				t.Stop()
			}
			d.mu.Unlock()
			d.wg.Wait()
			return
		case id := <-d.due:
			d.mu.Lock()
			delete(d.waiting, id)
			d.jobFailed(ctx, id)
			d.mu.Unlock()
		case c := <-d.settings:
			d.settingsChanged(c.New)
		case job := <-d.jobs:
			d.jobDone(ctx, job)
		case v := <-d.servers:
			d.serverState(ctx, v)
		case s := <-d.stats:
			d.hostStats(ctx, s)
		}
	}
}

func (d *Dispatcher) settingsChanged(st store.Settings) {
	d.mu.Lock()
	d.minFree = st.MinFreeDiskPercent
	d.mu.Unlock()
}

// serverState sends an alert when a server becomes unreachable, and the
// next one after the server was online again.
func (d *Dispatcher) serverState(ctx context.Context, v events.ServerState) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch v.Status {
	case store.ServerUnreachable:
		if d.down[v.ID] {
			return
		}
		d.down[v.ID] = true
		lastSeen := d.now()
		if v.LastSeenAt != nil {
			lastSeen = *v.LastSeenAt
		}
		d.goNotify(ctx, func(dash string) Event { return ServerUnreachableEvent(dash, v.ID, v.Name, lastSeen) })
	case store.ServerOnline:
		delete(d.down, v.ID)
	}
}

// jobDone sends the alert of a job that failed when its run data is there,
// or when the wait for the data ends.
func (d *Dispatcher) jobDone(ctx context.Context, job events.JobState) {
	if job.State != store.JobCompleted || job.Result != store.ResultFailed {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, sent := d.sent[job.ID]; sent {
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
		d.waiting[job.ID] = time.AfterFunc(enrichWait, func() {
			select {
			case d.due <- job.ID:
			case <-ctx.Done():
			}
		})
	}
}

// jobFailed sends the alert of a failed job with the job data of the
// store, one time for each job. d.mu must be held.
func (d *Dispatcher) jobFailed(ctx context.Context, id string) {
	now := d.now()
	if _, ok := d.sent[id]; ok {
		return
	}
	for job, at := range d.sent {
		if now.Sub(at) > 24*time.Hour {
			delete(d.sent, job)
		}
	}
	d.sent[id] = now
	d.goNotify(ctx, func(dash string) Event {
		job, err := d.st.GetJob(ctx, id)
		if err != nil {
			d.log.Warn("read the failed job", "job", id, "err", err)
			job = store.Job{ID: id}
		}
		return JobFailedEvent(dash, job)
	})
}

// hostStats sends a disk.low alert when the free disk space of a server
// falls below min_free_disk_percent. The next alert waits until the space
// rises diskMargin points above the limit and falls again, and at least
// diskEvery after the last alert of the server.
func (d *Dispatcher) hostStats(ctx context.Context, s events.HostStats) {
	free := 100 - s.DiskPercent
	now := d.now()
	d.mu.Lock()
	defer d.mu.Unlock()
	minFree := d.minFree
	fell := free < minFree && !d.diskLow[s.ServerID]
	switch {
	case free < minFree:
		d.diskLow[s.ServerID] = true
	case free >= minFree+diskMargin:
		delete(d.diskLow, s.ServerID)
	}
	if last, ok := d.diskSent[s.ServerID]; !fell || ok && now.Sub(last) < diskEvery {
		return
	}
	d.diskSent[s.ServerID] = now
	d.log.Warn("disk almost full", "server_id", s.ServerID, "free_percent", free)
	d.goNotify(ctx, func(dash string) Event {
		name := s.ServerID
		if v, err := d.st.GetServer(ctx, s.ServerID); err == nil {
			name = v.Name
		}
		return DiskLowEvent(dash, s.ServerID, name, free, minFree)
	})
}

// goNotify builds an event with the dashboard URL and sends it in the background.
func (d *Dispatcher) goNotify(ctx context.Context, build func(dashboard string) Event) {
	d.wg.Go(func() {
		st, err := d.st.GetSettings(ctx)
		if err != nil {
			d.log.Warn("read settings for a notification", "err", err)
		}
		e := build(st.DashboardURL())
		if err := d.Notify(ctx, e); err != nil {
			d.log.Warn("send notification", "type", e.Type, "err", err)
		}
	})
}
