package events

import (
	"sync"
	"testing"
	"time"
)

func TestSlowSubscriberDoesNotBlock(t *testing.T) {
	var b Bus
	slow, cancelSlow := b.Subscribe(1)
	defer cancelSlow()
	fast, cancelFast := b.Subscribe(10)
	defer cancelFast()

	done := make(chan struct{})
	go func() {
		for i := range 5 {
			b.Publish(Event{Type: JobState, Data: i})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a full subscriber")
	}

	if e := <-slow; e.Data != 0 {
		t.Errorf("slow got %v, want the first event", e.Data)
	}
	if n := b.Dropped(); n != 4 {
		t.Errorf("dropped = %d, want 4", n)
	}
	for i := range 5 {
		if e := <-fast; e.Data != i || e.Type != JobState {
			t.Fatalf("fast event %d = %+v", i, e)
		}
	}
}

func TestCancel(t *testing.T) {
	var b Bus
	ch, cancel := b.Subscribe(4)
	cancel()
	cancel() // a second call is safe
	b.Publish(Event{Type: SandboxState})
	if _, ok := <-ch; ok {
		t.Fatal("the channel is open after cancel")
	}
	if b.Dropped() != 0 {
		t.Fatal("an event was sent to a cancelled subscriber")
	}
}

func TestPublishWhileCancel(t *testing.T) {
	var b Bus
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				b.Publish(Event{Type: HostStats})
			}
		}
	}()
	for range 200 {
		_, cancel := b.Subscribe(0)
		cancel()
	}
	close(stop)
	wg.Wait()
}
