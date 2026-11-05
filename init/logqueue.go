package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/runplane/runplane/internal/sandbox/guest"
)

// logQueueMax is the memory for log lines while the host is away.
const logQueueMax = 8 << 20

var errLinkDown = errors.New("control connection dropped")

// logQueue holds log lines for the host in the form of the log stream
// (guest.AppendLogLine). When it is full, it drops the oldest lines.
type logQueue struct {
	max int

	mu      sync.Mutex
	lines   [][]byte
	size    int
	dropped int
	closed  bool
	wake    chan struct{} // closed and replaced on each change
}

func newLogQueue(max int) *logQueue {
	return &logQueue{max: max, wake: make(chan struct{})}
}

func (q *logQueue) push(source, text string) {
	b := guest.AppendLogLine(nil, guest.LogLine{Time: time.Now(), Source: source, Text: text})
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.lines = append(q.lines, b)
	q.size += len(b)
	for q.size > q.max && len(q.lines) > 0 {
		q.size -= len(q.lines[0])
		q.lines[0] = nil
		q.lines = q.lines[1:]
		q.dropped++
	}
	q.signal()
}

// close stops new lines. Readers get io.EOF after the last line.
func (q *logQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	q.signal()
}

func (q *logQueue) signal() {
	close(q.wake)
	q.wake = make(chan struct{})
}

// take waits for lines and returns all of them. It returns io.EOF when the
// queue is closed and empty, and errLinkDown when done closes first. After
// done closes, it leaves the lines in the queue for the next connection.
func (q *logQueue) take(done <-chan struct{}) ([]byte, error) {
	for {
		select {
		case <-done:
			return nil, errLinkDown
		default:
		}
		q.mu.Lock()
		if len(q.lines) > 0 || q.dropped > 0 {
			var b []byte
			if q.dropped > 0 {
				b = guest.AppendLogLine(b, guest.LogLine{
					Time:   time.Now(),
					Source: guest.SourceInit,
					Text:   fmt.Sprintf("dropped %d older log lines: the log buffer was full", q.dropped),
				})
				q.dropped = 0
			}
			for _, line := range q.lines {
				b = append(b, line...)
			}
			q.lines, q.size = nil, 0
			q.mu.Unlock()
			return b, nil
		}
		closed, wake := q.closed, q.wake
		q.mu.Unlock()
		if closed {
			return nil, io.EOF
		}
		select {
		case <-wake:
		case <-done:
			return nil, errLinkDown
		}
	}
}

// reader returns the queue as a stream for one connection. Lines that it
// took when the connection drops are lost.
func (q *logQueue) reader(done <-chan struct{}) io.Reader {
	return &queueReader{q: q, done: done}
}

type queueReader struct {
	q    *logQueue
	done <-chan struct{}
	buf  []byte
}

func (r *queueReader) Read(p []byte) (int, error) {
	if len(r.buf) == 0 {
		b, err := r.q.take(r.done)
		if err != nil {
			return 0, err
		}
		r.buf = b
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func (q *logQueue) writer(source string) io.Writer {
	return lineWriter{q: q, source: source}
}

type lineWriter struct {
	q      *logQueue
	source string
}

func (w lineWriter) Write(p []byte) (int, error) {
	for line := range strings.SplitSeq(strings.TrimSuffix(string(p), "\n"), "\n") {
		w.q.push(w.source, line)
	}
	return len(p), nil
}
