// Package events is the in-process publish and subscribe bus. The API sends
// the events to the browser over SSE.
package events

import (
	"sync"
	"sync/atomic"
)

// Event types. All except SettingsChanged are SSE event names.
const (
	SandboxState  = "sandbox.state"
	JobState      = "job.state"
	PoolStats     = "pool.stats"
	ServerState   = "server.state"
	HostStats     = "host.stats"
	RunnerVersion = "runner.version"
	ImageState    = "image.state"
	// GitHubState says that the GitHub status changed: the app, the
	// installations, or the scale set listeners. It has no data.
	GitHubState = "github.state"
	BackupState = "backup.state"

	// SettingsChanged is internal: packages that cache settings read them again.
	SettingsChanged = "settings.changed"
)

type Event struct {
	Type string
	Data any
}

// Bus delivers each event to all subscribers. The zero value is ready to use.
type Bus struct {
	mu      sync.RWMutex
	subs    map[chan Event]struct{}
	dropped atomic.Uint64
}

// Publish never blocks. A subscriber with a full buffer loses the event.
func (b *Bus) Publish(e Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.subs {
		select {
		case ch <- e:
		default:
			b.dropped.Add(1)
		}
	}
}

// Subscribe returns a channel with room for buffer events. Cancel stops the
// delivery and closes the channel; it is safe to call more than one time.
func (b *Bus) Subscribe(buffer int) (<-chan Event, func()) {
	ch := make(chan Event, buffer)
	b.mu.Lock()
	if b.subs == nil {
		b.subs = make(map[chan Event]struct{})
	}
	b.subs[ch] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, ch)
			b.mu.Unlock()
			close(ch)
		})
	}
	return ch, cancel
}

// Dropped is the count of events that subscribers lost because their buffers were full.
func (b *Bus) Dropped() uint64 { return b.dropped.Load() }
