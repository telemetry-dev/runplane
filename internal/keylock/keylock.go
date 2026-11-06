// Package keylock lets one caller at a time work on each key.
package keylock

import (
	"context"
	"sync"
)

// The zero value of Map is ready to use.
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

// Idle waits until no key is locked or ctx ends. Then it runs f and returns
// its error, and new Lock calls wait until f returns.
func (k *Map) Idle(ctx context.Context, f func() error) error {
	for {
		k.mu.Lock()
		if len(k.busy) == 0 {
			err := f()
			k.mu.Unlock()
			return err
		}
		var wait chan struct{}
		for _, done := range k.busy {
			wait = done
			break
		}
		k.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
