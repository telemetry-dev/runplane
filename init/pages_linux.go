//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/runplane/runplane/internal/sandbox/control"
)

// tailPages streams the lines of the job log pages that the runner writes
// to dir (_diag/pages) until ctx ends. The runner deletes a page soon after
// it uploads it, so tailPages opens each page as soon as it appears and
// reads it through the open file.
func tailPages(ctx context.Context, dir string, emit func(string)) error {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return fmt.Errorf("inotify: %w", err)
	}
	events := os.NewFile(uintptr(fd), "inotify") // nonblocking, so reads use the poller
	defer events.Close()
	if _, err := unix.InotifyAddWatch(fd, dir, unix.IN_CREATE|unix.IN_MOVED_TO|unix.IN_MODIFY|unix.IN_CLOSE_WRITE|unix.IN_DELETE); err != nil {
		return fmt.Errorf("watch %s: %w", dir, err)
	}
	stop := context.AfterFunc(ctx, func() { events.Close() })
	defer stop()

	t := &pageTail{dir: dir, emit: emit, open: map[string]*page{}, seen: map[string]bool{}}
	t.scan()
	buf := make([]byte, 64<<10)
	for {
		n, err := events.Read(buf)
		if err != nil {
			// ctx ended: the runner has exited, so the pages are complete.
			t.scan()
			for name := range t.open {
				t.finish(name)
			}
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		for off := 0; off+unix.SizeofInotifyEvent <= n; {
			mask := binary.NativeEndian.Uint32(buf[off+4:])
			size := int(binary.NativeEndian.Uint32(buf[off+12:]))
			name := string(bytes.TrimRight(buf[off+unix.SizeofInotifyEvent:off+unix.SizeofInotifyEvent+size], "\x00"))
			off += unix.SizeofInotifyEvent + size
			switch {
			case mask&unix.IN_Q_OVERFLOW != 0:
				t.scan()
			case mask&(unix.IN_CREATE|unix.IN_MOVED_TO) != 0:
				t.start(name)
			case mask&unix.IN_MODIFY != 0:
				if p := t.open[name]; p != nil {
					p.read(t.emit)
				}
			case mask&(unix.IN_CLOSE_WRITE|unix.IN_DELETE) != 0:
				t.finish(name)
			}
		}
	}
}

type pageTail struct {
	dir  string
	emit func(string)
	open map[string]*page
	seen map[string]bool // each page name, so that a scan reads no page twice
}

// scan picks up pages that appeared without an event.
func (t *pageTail) scan() {
	entries, _ := os.ReadDir(t.dir)
	for _, e := range entries {
		t.start(e.Name())
	}
}

func (t *pageTail) start(name string) {
	if !strings.HasSuffix(name, ".log") || t.seen[name] {
		return
	}
	f, err := os.Open(filepath.Join(t.dir, name))
	if err != nil {
		return
	}
	t.seen[name] = true
	p := &page{f: f}
	t.open[name] = p
	p.read(t.emit)
}

// finish reads the rest of a page that the runner closed or deleted.
func (t *pageTail) finish(name string) {
	p := t.open[name]
	if p == nil {
		return
	}
	p.read(t.emit)
	if len(p.partial) > 0 {
		t.emit(string(p.partial))
	}
	p.f.Close()
	delete(t.open, name)
}

type page struct {
	f       *os.File
	partial []byte // text after the last line end
	started bool
}

func (p *page) read(emit func(string)) {
	buf := make([]byte, 32<<10)
	for {
		n, err := p.f.Read(buf)
		data := buf[:n]
		if !p.started && n > 0 {
			// .NET writes a UTF-8 byte order mark at the start of the page.
			data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
			p.started = true
		}
		p.partial = append(p.partial, data...)
		for {
			i := bytes.IndexByte(p.partial, '\n')
			if i < 0 {
				break
			}
			emit(string(bytes.TrimSuffix(p.partial[:i], []byte("\r"))))
			p.partial = p.partial[i+1:]
		}
		if len(p.partial) >= control.MaxLine {
			emit(string(p.partial))
			p.partial = nil
		}
		if err != nil || n == 0 {
			return
		}
	}
}
