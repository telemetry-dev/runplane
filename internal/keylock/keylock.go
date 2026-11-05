// Package keylock lets one caller at a time work on each key.
package keylock

import (
	"context"
	"sync"
)

// Map holds a lock for each key. The zero value is ready to use.
type Map struct {
	mu   sync.Mutex
	busy map[string]chan struct{} // closed when the key is free again
}

// Lock waits until key is free or ctx ends. The caller calls unlock when
// its work on key is done.
func (k *Map) Lock(ctx context.Context, key string) (unlock func(), err error) {
	for {
		k.mu.Lock()
		wait, ok := k.busy[key]
		if !ok {
			if k.busy == nil {
				k.busy = map[string]chan struct{}{}
			}
			done := make(chan struct{})
			k.busy[key] = done
			k.mu.Unlock()
			return func() {
				k.mu.Lock()
				delete(k.busy, key)
				k.mu.Unlock()
				close(done)
			}, nil
		}
		k.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Idle runs f if no key is locked, and keeps new Lock calls waiting until f
// returns. It reports whether f ran.
func (k *Map) Idle(f func()) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.busy) > 0 {
		return false
	}
	f()
	return true
}
