package control

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/runplane/runplane/internal/sandbox/guest"
)

const logMax = 64 << 20 // at this size, console.log moves to console.log.1

const logTime = "2006-01-02T15:04:05.000Z07:00"

// LogFile is the console log of one sandbox: <dir>/console.log and the older
// part console.log.1. Each line is "<RFC 3339 UTC time> <source> <text>"
// (ParseLogFileLine).
type LogFile struct {
	path string

	mu      sync.Mutex
	f       *os.File
	size    int64
	part    int           // counts the rotations
	written chan struct{} // closed and replaced on each write
	closed  bool
}

func OpenLogFile(dir string) (*LogFile, error) {
	path := filepath.Join(dir, "console.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open console log: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("stat console log: %w", err)
	}
	return &LogFile{path: path, f: f, size: st.Size(), written: make(chan struct{})}, nil
}

func (l *LogFile) Write(line guest.LogLine) error {
	b := make([]byte, 0, 32+len(line.Source)+len(line.Text))
	b = line.Time.UTC().AppendFormat(b, logTime)
	b = append(b, ' ')
	b = append(b, line.Source...)
	b = append(b, ' ')
	b = append(b, line.Text...)
	b = append(b, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return os.ErrClosed
	}
	if l.size > 0 && l.size+int64(len(b)) > logMax {
		if err := l.rotate(); err != nil {
			return err
		}
	}
	n, err := l.f.Write(b)
	l.size += int64(n)
	close(l.written)
	l.written = make(chan struct{})
	if err != nil {
		return fmt.Errorf("write console log: %w", err)
	}
	return nil
}

// ParseLogFileLine reads a line of a console log. It reports false for a
// line in another form.
func ParseLogFileLine(line string) (guest.LogLine, bool) {
	stamp, rest, ok1 := strings.Cut(line, " ")
	source, text, ok2 := strings.Cut(rest, " ")
	t, err := time.Parse(logTime, stamp)
	if !ok1 || !ok2 || err != nil {
		return guest.LogLine{}, false
	}
	return guest.LogLine{Time: t, Source: source, Text: text}, true
}

func (l *LogFile) rotate() error {
	l.f.Close()
	if err := os.Rename(l.path, l.path+".1"); err != nil {
		return fmt.Errorf("rotate console log: %w", err)
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("rotate console log: %w", err)
	}
	l.f = f
	l.size = 0
	l.part++
	return nil
}

// Close ends the log for good. Readers that follow it reach the end.
func (l *LogFile) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	close(l.written)
	return l.f.Close()
}

// Reader reads console.log.1 and console.log from the start. With follow, it
// then waits for new lines like tail -f, until ctx ends, the log closes, or
// the reader closes. An open error comes from the first Read.
func (l *LogFile) Reader(ctx context.Context, follow bool) io.ReadCloser {
	r := &logReader{l: l, ctx: ctx, follow: follow, stop: make(chan struct{})}
	l.mu.Lock()
	defer l.mu.Unlock()
	r.part = l.part
	f, err := os.Open(l.path)
	if err != nil {
		r.err = err
		return r
	}
	r.f = f
	if old, err := os.Open(l.path + ".1"); err == nil {
		r.f, r.next = old, []*os.File{f}
	}
	return r
}

type logReader struct {
	l         *LogFile
	ctx       context.Context
	follow    bool
	stop      chan struct{} // closed by Close
	closeOnce sync.Once

	mu   sync.Mutex
	f    *os.File
	next []*os.File // files to read after f
	part int        // rotation count when f was console.log
	err  error
}

func (r *logReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		if r.err != nil {
			return 0, r.err
		}
		// Take the state before the read. A write after this point closes
		// written, so the wait below cannot miss it.
		r.l.mu.Lock()
		written, part, closed := r.l.written, r.l.part, r.l.closed
		r.l.mu.Unlock()

		n, err := r.f.Read(p)
		if n > 0 || (err != nil && err != io.EOF) {
			return n, err
		}
		if len(r.next) > 0 {
			r.f.Close()
			r.f, r.next = r.next[0], r.next[1:]
			continue
		}
		if part != r.part {
			// The writer rotated before the read above, so the read
			// reached the true end of the old file.
			r.err = r.reopen()
			continue
		}
		if !r.follow || closed {
			return 0, io.EOF
		}
		r.mu.Unlock()
		select {
		case <-written:
			r.mu.Lock()
		case <-r.ctx.Done():
			r.mu.Lock()
			return 0, io.EOF
		case <-r.stop:
			r.mu.Lock()
			return 0, os.ErrClosed
		}
	}
}

func (r *logReader) reopen() error {
	r.l.mu.Lock()
	defer r.l.mu.Unlock()
	f, err := os.Open(r.l.path)
	if err != nil {
		return err
	}
	r.f.Close()
	r.f = f
	r.part = r.l.part
	return nil
}

// Close also ends a Read that waits for new lines.
func (r *logReader) Close() error {
	r.closeOnce.Do(func() { close(r.stop) })
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f != nil {
		r.f.Close()
	}
	for _, f := range r.next {
		f.Close()
	}
	r.err = os.ErrClosed
	return nil
}
