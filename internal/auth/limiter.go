package auth

import (
	"sync"
	"time"
)

// limiter blocks a key (an IP address or an email) after max failures in
// one window. It keeps the times of the last max failures of each key.
type limiter struct {
	max    int
	window time.Duration

	mu    sync.Mutex
	fails map[string][]time.Time
	swept time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{max: max, window: window, fails: map[string][]time.Time{}}
}

func (l *limiter) blocked(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent(key, now)) >= l.max
}

func (l *limiter) fail(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)
	times := append(l.recent(key, now), now)
	if len(times) > l.max {
		times = times[len(times)-l.max:]
	}
	l.fails[key] = times
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, key)
}

// recent returns the failures of key inside the window that ends at now.
func (l *limiter) recent(key string, now time.Time) []time.Time {
	times := l.fails[key]
	for len(times) > 0 && now.Sub(times[0]) >= l.window {
		times = times[1:]
	}
	return times
}

// sweep drops the keys with no failure in the window, at most one time per
// window, so the map does not grow without limit.
func (l *limiter) sweep(now time.Time) {
	if now.Sub(l.swept) < l.window {
		return
	}
	l.swept = now
	for key, times := range l.fails {
		if now.Sub(times[len(times)-1]) >= l.window {
			delete(l.fails, key)
		}
	}
}
