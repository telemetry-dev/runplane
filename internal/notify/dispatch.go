package notify

import (
	"cmp"
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/runplane/runplane/internal/events"
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

// Dispatcher sends the alerts to the notification channels that subscribe
// to them. Other packages give it alerts through Notify. Run makes the
// alerts of the bus: one job.failed alert for each failed job, one
// server.unreachable alert for each outage of a server, at most one
// disk.low alert for each server in 6 hours, and one backup.failed alert for
// each failed backup.
type Dispatcher struct {
	st       *store.Store
	channels *Channels
	log      *slog.Logger
	now      func() time.Time

	// ctx ends when Run ends. It holds the subscriptions and the sends.
	ctx      context.Context
	stop     context.CancelFunc
	settings <-chan events.SettingsChanged
	jobs     <-chan events.JobState
	servers  <-chan events.ServerState
	stats    <-chan events.HostStats
	backups  <-chan events.BackupState

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
	Store    *store.Store
	Channels *Channels
	Bus      *events.Bus
	Log      *slog.Logger
}

// New makes a dispatcher. It takes its events from the bus at once, also the
// events before Run starts.
func New(c Config) *Dispatcher {
	ctx, stop := context.WithCancel(context.Background())
	return &Dispatcher{
		st:       c.Store,
		channels: c.Channels,
		log:      cmp.Or(c.Log, slog.New(slog.DiscardHandler)),
		now:      time.Now,
		ctx:      ctx,
		stop:     stop,
		settings: events.Subscribe[events.SettingsChanged](ctx, c.Bus),
		jobs:     events.Subscribe[events.JobState](ctx, c.Bus),
		servers:  events.Subscribe[events.ServerState](ctx, c.Bus),
		stats:    events.Subscribe[events.HostStats](ctx, c.Bus),
		backups:  events.Subscribe[events.BackupState](ctx, c.Bus),
		due:      make(chan string),
		minFree:  store.DefaultSettings().MinFreeDiskPercent,
		sent:     map[string]time.Time{},
		waiting:  map[string]*time.Timer{},
		down:     map[string]bool{},
		diskLow:  map[string]bool{},
		diskSent: map[string]time.Time{},
	}
}

// Notify sends e in the background to each enabled channel that subscribes
// to its type, and records each delivery. The link of e goes to the
// dashboard URL of the settings.
func (d *Dispatcher) Notify(e Event) {
	d.notify(func() Event { return e })
}

// Run makes alerts from the bus until ctx ends.
func (d *Dispatcher) Run(ctx context.Context) {
	defer d.stop()
	if st, err := d.st.GetSettings(ctx); err == nil {
		d.settingsChanged(st)
	} else {
		d.log.Warn("read settings for notifications", "err", err)
	}
	for {
		select {
		case <-ctx.Done():
			d.mu.Lock()
			for _, t := range d.waiting {
				t.Stop()
			}
			d.mu.Unlock()
			d.stop()
			d.wg.Wait()
			return
		case id := <-d.due:
			d.mu.Lock()
			delete(d.waiting, id)
			d.jobFailed(id)
			d.mu.Unlock()
		case c := <-d.settings:
			d.settingsChanged(c.New)
		case job := <-d.jobs:
			d.jobDone(job)
		case v := <-d.servers:
			d.serverState(v)
		case s := <-d.stats:
			d.hostStats(s)
		case b := <-d.backups:
			if b.Status == store.BackupFailed {
				d.Notify(BackupFailedEvent(b.Error))
			}
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
func (d *Dispatcher) serverState(v events.ServerState) {
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
		d.Notify(ServerUnreachableEvent(v.ID, v.Name, lastSeen))
	case store.ServerOnline:
		delete(d.down, v.ID)
	}
}

// jobDone sends the alert of a job that failed when its run data is there,
// or when the wait for the data ends.
func (d *Dispatcher) jobDone(job events.JobState) {
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
		d.jobFailed(job.ID)
	case !waits:
		d.waiting[job.ID] = time.AfterFunc(enrichWait, func() {
			select {
			case d.due <- job.ID:
			case <-d.ctx.Done():
			}
		})
	}
}

// jobFailed sends the alert of a failed job with the job data of the
// store, one time for each job. d.mu must be held.
func (d *Dispatcher) jobFailed(id string) {
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
	d.notify(func() Event {
		job, err := d.st.GetJob(d.ctx, id)
		if err != nil {
			d.log.Warn("read the failed job", "job", id, "err", err)
			job = store.Job{ID: id}
		}
		return JobFailedEvent(job)
	})
}

// hostStats sends a disk.low alert when the free disk space of a server
// falls below min_free_disk_percent. The next alert waits until the space
// rises diskMargin points above the limit and falls again, and at least
// diskEvery after the last alert of the server.
func (d *Dispatcher) hostStats(s events.HostStats) {
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
	d.notify(func() Event {
		name := s.ServerID
		if v, err := d.st.GetServer(d.ctx, s.ServerID); err == nil {
			name = v.Name
		}
		return DiskLowEvent(s.ServerID, name, free, minFree)
	})
}

// notify builds an event and sends it in the background, with links to
// the dashboard URL of the settings.
func (d *Dispatcher) notify(build func() Event) {
	d.wg.Go(func() {
		st, err := d.st.GetSettings(d.ctx)
		if err != nil {
			d.log.Warn("read settings for a notification", "err", err)
		}
		e := build()
		if err := d.channels.notify(d.ctx, e, st.DashboardURL()); err != nil {
			d.log.Warn("send notification", "type", e.Type, "err", err)
		}
	})
}
