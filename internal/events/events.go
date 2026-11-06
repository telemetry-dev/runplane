// Package events is the in-process bus. Each event type of this package is a
// topic and carries its own data, so a publisher cannot send the data of
// another topic.
//
// The bus has one delivery rule for each kind of subscriber. Subscribe never
// loses an event, for the packages that must see each event: notify must see
// each failed job, and github each change of the settings and the pools.
// Watch ends when its client reads too slowly. The SSE stream of the API uses
// Watch, and the browser then connects again and reads the state from the API.
package events

import (
	"context"
	"sync"

	"github.com/runplane/runplane/internal/store"
)

// Event is an event of the bus. Only the types of this package are events.
type Event interface{ event() }

// Each of these events has a row after its change.
type (
	SandboxState  store.Sandbox
	JobState      store.Job
	ServerState   store.Server
	RunnerVersion store.RunnerBundle
	BackupState   store.Backup
)

// ServerRemoved has the ID of a removed server. The browser gets it as it is.
type ServerRemoved struct {
	ID string `json:"id"`
}

// HostStats is a new host sample of a server. The browser gets it as it is.
type HostStats store.ServerStat

// ImageState is an image row after a change.
type ImageState store.Image

// ImageRemoved has the ID of a removed image. The browser gets it as it is.
type ImageRemoved struct {
	ID string `json:"id"`
}

// ImageBuildState is the build of an image on a server after a change.
type ImageBuildState store.ImageBuild

// PoolStats counts the runners and the waiting jobs of a pool. The browser
// gets it as it is.
type PoolStats struct {
	PoolID string `json:"pool_id"`
	store.PoolCounts
}

// PoolsChanged says that the pools changed, or what they use: a pool, the
// default image, an image, or a server that a pool names. The browser gets
// it as it is.
type PoolsChanged struct{}

// GitHubState says that the GitHub status changed: the app, the
// installations, or the scale set listeners.
type GitHubState struct{}

// SettingsChanged has the settings before and after a change. The browser
// gets the API object of the new settings.
type SettingsChanged struct{ Old, New store.Settings }

func (SandboxState) event()    {}
func (JobState) event()        {}
func (ServerState) event()     {}
func (ServerRemoved) event()   {}
func (HostStats) event()       {}
func (RunnerVersion) event()   {}
func (BackupState) event()     {}
func (ImageState) event()      {}
func (ImageRemoved) event()    {}
func (ImageBuildState) event() {}
func (PoolStats) event()       {}
func (PoolsChanged) event()    {}
func (GitHubState) event()     {}
func (SettingsChanged) event() {}

// Bus gives each event to its subscribers. The zero value is ready to use.
type Bus struct {
	mu    sync.RWMutex
	sinks map[*sink]struct{}
}

// sink takes the events of one subscriber. Its put never waits.
type sink struct{ put func(Event) }

// Publish gives e to each subscriber. It never waits for a subscriber.
func (b *Bus) Publish(e Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for s := range b.sinks {
		s.put(e)
	}
}

// Subscribe returns each event of type E until ctx ends, in the order of
// Publish. It never loses an event: when the subscriber is slow, its events
// wait in a queue without a size limit. The channel does not close.
func Subscribe[E Event](ctx context.Context, b *Bus) <-chan E {
	q := &queue[E]{ch: make(chan E, queueRoom), done: ctx.Done()}
	b.add(ctx, func(e Event) {
		if x, ok := e.(E); ok {
			q.push(x)
		}
	})
	return q.ch
}

// Watch returns all events until ctx ends, for a client that can read the
// state again. When more than buffer events wait for the client, the client
// loses the events and the channel closes, so that the client knows.
func (b *Bus) Watch(ctx context.Context, buffer int) <-chan Event {
	ch := make(chan Event, buffer)
	var mu sync.Mutex
	closed := false
	b.add(ctx, func(e Event) {
		mu.Lock()
		defer mu.Unlock()
		if closed {
			return
		}
		select {
		case ch <- e:
		default:
			closed = true
			close(ch)
		}
	})
	return ch
}

func (b *Bus) add(ctx context.Context, put func(Event)) {
	s := &sink{put}
	b.mu.Lock()
	if b.sinks == nil {
		b.sinks = map[*sink]struct{}{}
	}
	b.sinks[s] = struct{}{}
	b.mu.Unlock()
	context.AfterFunc(ctx, func() {
		b.mu.Lock()
		delete(b.sinks, s)
		b.mu.Unlock()
	})
}

// queueRoom is the room of a Subscribe channel. More events wait in the queue.
const queueRoom = 64

type queue[E any] struct {
	ch   chan E
	done <-chan struct{}

	mu   sync.Mutex
	wait []E // the events after a full ch, in order
}

func (q *queue[E]) push(x E) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.wait) == 0 {
		select {
		case q.ch <- x:
			return
		default:
			go q.drain()
		}
	}
	q.wait = append(q.wait, x)
}

// drain moves the waiting events to ch. One drain runs while wait has
// events: push starts it for the first one, and it stops when wait is empty.
func (q *queue[E]) drain() {
	q.mu.Lock()
	for len(q.wait) > 0 {
		x := q.wait[0]
		q.mu.Unlock()
		select {
		case q.ch <- x:
		case <-q.done:
			return
		}
		q.mu.Lock()
		q.wait = q.wait[1:]
	}
	q.wait = nil
	q.mu.Unlock()
}
