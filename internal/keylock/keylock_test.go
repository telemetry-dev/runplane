package keylock

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The image GC and the runner prune use Idle. A GC that does not wait for
// the work on a key, or that never runs, keeps unused images on the disk.
func TestIdleWaitsForTheLockedKeys(t *testing.T) {
	var k Map
	unlock, err := k.Lock(t.Context(), "builtin:ubuntu-24.04")
	if err != nil {
		t.Fatal(err)
	}
	ran := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- k.Idle(t.Context(), func() error {
			close(ran)
			return errors.New("gc failed")
		})
	}()
	select {
	case <-ran:
		t.Fatal("Idle ran while a key was locked")
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	select {
	case err := <-done:
		if err == nil || err.Error() != "gc failed" {
			t.Errorf("Idle = %v, want the error of f", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Idle did not run after the unlock")
	}
}

func TestIdleEndsWithItsContext(t *testing.T) {
	var k Map
	unlock, err := k.Lock(t.Context(), "builtin:ubuntu-24.04")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	ran := false
	if err := k.Idle(ctx, func() error { ran = true; return nil }); !errors.Is(err, context.Canceled) || ran {
		t.Errorf("Idle with an ended context = %v, ran %v; want context.Canceled and no run", err, ran)
	}
}
