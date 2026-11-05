package image

import (
	"context"
	"io"
	"sync"
	"time"
)

// reporter sends progress to a callback, one call at a time. It sends each
// status change and each completed count, and other byte counts at most twice
// a second.
type reporter struct {
	fn   func(Progress)
	mu   sync.Mutex
	last Progress
	at   time.Time
}

func (r *reporter) status(s Status) { r.send(Progress{Status: s}) }

func (r *reporter) bytes(s Status, done, total int64) {
	r.send(Progress{Status: s, Done: done, Total: total})
}

func (r *reporter) send(p Progress) {
	if r.fn == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if p.Status == r.last.Status && p.Done != p.Total && now.Sub(r.at) < 500*time.Millisecond {
		return
	}
	r.last, r.at = p, now
	r.fn(p)
}

// counter stops when ctx ends.
type counter struct {
	ctx context.Context
	r   io.Reader
	add func(n int64)
}

func (c *counter) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := c.r.Read(p)
	c.add(int64(n))
	return n, err
}

type readCloser struct {
	io.Reader
	io.Closer
}
