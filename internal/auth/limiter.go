package auth

import (
	"sync"
	"time"
)

const (
	maxFailures = 10
	failWindow  = 15 * time.Minute
)

// limiter blocks a key (an IP address or an email) after maxFailures in
// failWindow. It keeps the times of the last maxFailures failures of each key.
type limiter struct {
	mu    sync.Mutex
	fails map[string][]time.Time
	swept time.Time
}

func (l *limiter) blocked(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent(key, now)) >= maxFailures
}

func (l *limiter) fail(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)
	times := append(l.recent(key, now), now)
	if len(times) > maxFailures {
		times = times[len(times)-maxFailures:]
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
	for len(times) > 0 && now.Sub(times[0]) >= failWindow {
		times = times[1:]
	}
	return times
}

// sweep drops the keys with no failure in the window, at most one time per
// window, so the map does not grow without limit.
func (l *limiter) sweep(now time.Time) {
	if now.Sub(l.swept) < failWindow {
		return
	}
	l.swept = now
	for key, times := range l.fails {
		if now.Sub(times[len(times)-1]) >= failWindow {
			delete(l.fails, key)
		}
	}
}
