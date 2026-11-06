package events

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/store"
)

func TestSlowSubscriberDoesNotBlock(t *testing.T) {
	var b Bus
	ctx := t.Context()
	n := 3 * queueRoom
	stats := Subscribe[PoolStats](ctx, &b)
	slow := b.Watch(ctx, 1)
	fast := b.Watch(ctx, 2*n)

	done := make(chan struct{})
	go func() {
		for i := range n {
			b.Publish(PoolStats{PoolCounts: store.PoolCounts{Idle: i}})
			b.Publish(GitHubState{})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a full subscriber")
	}

	for i := range n {
		select {
		case p := <-stats:
			if p.Idle != i {
				t.Fatalf("pool.stats event %d has idle %d, want the events in order", i, p.Idle)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("got %d of %d pool.stats events", i, n)
		}
	}
	select {
	case p := <-stats:
		t.Errorf("one more pool.stats event %+v, want none", p)
	default:
	}

	if e := <-slow; e != (PoolStats{PoolCounts: store.PoolCounts{Idle: 0}}) {
		t.Errorf("slow watcher got %+v, want the first event", e)
	}
	select {
	case e, ok := <-slow:
		if ok {
			t.Errorf("slow watcher got %+v, want its channel closed after it lost events", e)
		}
	case <-time.After(5 * time.Second):
		t.Error("the channel of the slow watcher is open after it lost events")
	}
	for i := range 2 * n {
		if _, ok := <-fast; !ok {
			t.Fatalf("fast watcher closed after %d events", i)
		}
	}
}

// Publish puts an event in the channel of each subscriber before it
// returns, so an event that does not come shows that the subscription ended.
func TestSubscriptionEndsWithContext(t *testing.T) {
	for _, tt := range []struct {
		name string
		// subscribe returns got, which reports if an event waits for the subscriber.
		subscribe func(t *testing.T, ctx context.Context, b *Bus) (got func() bool)
	}{
		{"Subscribe", func(t *testing.T, ctx context.Context, b *Bus) func() bool {
			ch := Subscribe[JobState](ctx, b)
			return func() bool {
				select {
				case <-ch:
					return true
				default:
					return false
				}
			}
		}},
		{"Watch", func(t *testing.T, ctx context.Context, b *Bus) func() bool {
			ch := b.Watch(ctx, 1)
			return func() bool {
				select {
				case _, ok := <-ch:
					if !ok {
						t.Fatal("the channel closed, so the test cannot see more events")
					}
					return true
				default:
					return false
				}
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var b Bus
			ctx, cancel := context.WithCancel(t.Context())
			got := tt.subscribe(t, ctx, &b)
			cancel()
			deadline := time.Now().Add(5 * time.Second)
			for b.Publish(JobState{}); got(); b.Publish(JobState{}) {
				if time.Now().After(deadline) {
					t.Fatal("the subscriber gets events after the end of its context")
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}

func TestPublishWhileCancel(t *testing.T) {
	var b Bus
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				b.Publish(HostStats{})
			}
		}
	})
	for range 200 {
		ctx, cancel := context.WithCancel(t.Context())
		b.Watch(ctx, 0)
		Subscribe[HostStats](ctx, &b)
		cancel()
	}
	close(stop)
	wg.Wait()
}
