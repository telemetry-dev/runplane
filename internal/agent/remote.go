package agent

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
)

var _ sandbox.Driver = (*RemoteDriver)(nil)

// RemoteDriver is the sandbox driver of an agent. Each call goes to the
// agent over its Session. The JIT config of a sandbox goes to the agent only
// in the Start call, and the agent keeps it in memory.
type RemoteDriver struct {
	s *Session
}

// Mode is the mode that the agent sent in its hello.
func (d *RemoteDriver) Mode() sandbox.Mode { return d.s.hello.Mode }

func (d *RemoteDriver) Start(ctx context.Context, spec sandbox.Spec) error {
	return d.s.call(ctx, http.MethodPost, "/v1/sandboxes", spec, nil)
}

func (d *RemoteDriver) Stop(ctx context.Context, id string) error {
	return d.s.call(ctx, http.MethodDelete, sandboxPath(id, ""), nil, nil)
}

func (d *RemoteDriver) List(ctx context.Context) ([]sandbox.Sandbox, error) {
	var list []sandbox.Sandbox
	err := d.s.call(ctx, http.MethodGet, "/v1/sandboxes", nil, &list)
	return list, err
}

func (d *RemoteDriver) Capacity(ctx context.Context) (sandbox.Capacity, error) {
	var c sandbox.Capacity
	err := d.s.call(ctx, http.MethodGet, "/v1/capacity", nil, &c)
	return c, err
}

// Events delivers the driver events of the agent. It closes when the
// session ends.
func (d *RemoteDriver) Events() <-chan sandbox.Event { return d.s.events }

// Logs streams the log of a sandbox. With follow, the stream stays open
// until ctx ends, the reader closes, or the session ends.
func (d *RemoteDriver) Logs(ctx context.Context, id string, follow bool) (io.ReadCloser, error) {
	path := sandboxPath(id, "/logs")
	if follow {
		path += "?follow=1"
	}
	resp, err := d.s.send(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Shell opens a debug shell. ctx limits only the open; the terminal stays
// open until Close, the end of the shell, or the end of the session.
func (d *RemoteDriver) Shell(ctx context.Context, id string, size sandbox.TermSize) (sandbox.Terminal, error) {
	q := url.Values{
		"rows": {strconv.Itoa(int(size.Rows))},
		"cols": {strconv.Itoa(int(size.Cols))},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://agent"+sandboxPath(id, "/shell")+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", shellProtocol)
	resp, err := d.s.do(req)
	if err != nil {
		return nil, err
	}
	rwc, ok := resp.Body.(io.ReadWriteCloser)
	if resp.StatusCode != http.StatusSwitchingProtocols || !ok {
		resp.Body.Close()
		return nil, fmt.Errorf("open shell on server %s: status %s", d.s.ServerID(), resp.Status)
	}
	return &remoteTerminal{rwc: rwc}, nil
}

func (d *RemoteDriver) Hold(ctx context.Context, id string, until time.Time) error {
	return d.s.call(ctx, http.MethodPost, sandboxPath(id, "/hold"), holdRequest{Until: until.UTC()}, nil)
}

// Close ends the session. The sandboxes on the agent keep operating.
func (d *RemoteDriver) Close() error { return d.s.Close() }

func sandboxPath(id, suffix string) string {
	return "/v1/sandboxes/" + url.PathEscape(id) + suffix
}

// The shell stream after the upgrade: the agent sends the raw terminal
// output. The primary sends frames, so that a resize can go on the same
// stream as the input. A frame is one kind byte, a big-endian uint16 length,
// and the payload. A resize payload is rows and cols as big-endian uint16.
const (
	shellProtocol      = "runplane-shell"
	frameData     byte = 0
	frameResize   byte = 1
	maxFrame           = 32 << 10
)

type remoteTerminal struct {
	rwc io.ReadWriteCloser
	mu  sync.Mutex // one frame at a time
}

func (t *remoteTerminal) Read(p []byte) (int, error) { return t.rwc.Read(p) }

func (t *remoteTerminal) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		chunk := p[:min(len(p), maxFrame)]
		if err := t.frame(frameData, chunk); err != nil {
			return n, err
		}
		n += len(chunk)
		p = p[len(chunk):]
	}
	return n, nil
}

func (t *remoteTerminal) Resize(size sandbox.TermSize) error {
	var b [4]byte
	binary.BigEndian.PutUint16(b[0:2], size.Rows)
	binary.BigEndian.PutUint16(b[2:4], size.Cols)
	return t.frame(frameResize, b[:])
}

func (t *remoteTerminal) Close() error { return t.rwc.Close() }

func (t *remoteTerminal) frame(kind byte, payload []byte) error {
	buf := make([]byte, 3+len(payload))
	buf[0] = kind
	binary.BigEndian.PutUint16(buf[1:3], uint16(len(payload)))
	copy(buf[3:], payload)
	t.mu.Lock()
	defer t.mu.Unlock()
	_, err := t.rwc.Write(buf)
	return err
}

// readFrames passes the frames from r to term until r ends.
func readFrames(r io.Reader, term sandbox.Terminal) error {
	var hdr [3]byte
	buf := make([]byte, maxFrame)
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return err
		}
		n := int(binary.BigEndian.Uint16(hdr[1:3]))
		if n > maxFrame {
			return fmt.Errorf("shell frame of %d bytes is too long", n)
		}
		payload := buf[:n]
		if _, err := io.ReadFull(r, payload); err != nil {
			return err
		}
		switch hdr[0] {
		case frameData:
			if _, err := term.Write(payload); err != nil {
				return err
			}
		case frameResize:
			if n != 4 {
				return errors.New("bad shell resize frame")
			}
			// A failed resize leaves the old size; the shell goes on.
			term.Resize(sandbox.TermSize{
				Rows: binary.BigEndian.Uint16(payload[0:2]),
				Cols: binary.BigEndian.Uint16(payload[2:4]),
			})
		default:
			return fmt.Errorf("unknown shell frame kind %d", hdr[0])
		}
	}
}
