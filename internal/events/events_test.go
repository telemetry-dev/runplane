package events

import (
	"context"
	"sync"
	"testing"
	"time"
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
			b.Publish(PoolStats{Idle: i})
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

	if e := <-slow; e != (PoolStats{Idle: 0}) {
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

func TestSubscriptionEndsWithContext(t *testing.T) {
	var b Bus
	ctx, cancel := context.WithCancel(t.Context())
	Subscribe[JobState](ctx, &b)
	b.Watch(ctx, 4)
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for subscribers(&b) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d subscribers after the end of their context", subscribers(&b))
		}
		time.Sleep(time.Millisecond)
	}
}

func subscribers(b *Bus) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.sinks)
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
