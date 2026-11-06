package agent

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
)

type remoteDriver struct{ s *Session }

func (d remoteDriver) Mode() sandbox.Mode { return d.s.hello.Mode }

// Start is the only call that sends the JIT config of a sandbox to the
// agent, and the agent keeps it in memory.
func (d remoteDriver) Start(ctx context.Context, spec sandbox.Spec) error {
	_, err := call(ctx, d.s, opStart, spec)
	return err
}

func (d remoteDriver) Stop(ctx context.Context, id string) error {
	_, err := call(ctx, d.s, opStop, sandboxID(id))
	return err
}

func (d remoteDriver) List(ctx context.Context) ([]sandbox.Sandbox, error) {
	return call(ctx, d.s, opList, none{})
}

func (d remoteDriver) Capacity(ctx context.Context) (sandbox.Capacity, error) {
	return call(ctx, d.s, opCapacity, none{})
}

// Configure sends the driver settings. The agent keeps the
// min_free_disk_percent of the last Session.Configure.
func (d remoteDriver) Configure(ctx context.Context, settings sandbox.Settings) error {
	d.s.settingsMu.Lock()
	s := d.s.settings
	d.s.settingsMu.Unlock()
	s.Settings = settings
	return d.s.Configure(ctx, s)
}

// Events closes its channel when the session ends.
func (d remoteDriver) Events() <-chan sandbox.Event { return d.s.events }

// With follow, Logs keeps the stream open until ctx ends, the reader
// closes, or the session ends.
func (d remoteDriver) Logs(ctx context.Context, id string, follow bool) (io.ReadCloser, error) {
	resp, err := open(ctx, d.s, opLogs.route, logsRequest{ID: id, Follow: follow})
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Shell opens a debug shell. ctx limits only the open; the terminal stays
// open until Close, the end of the shell, or the end of the session.
func (d remoteDriver) Shell(ctx context.Context, id string, size sandbox.TermSize) (sandbox.Terminal, error) {
	req, err := opShell.request(ctx, shellRequest{ID: id, Size: size})
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

func (d remoteDriver) Hold(ctx context.Context, id string, until time.Time) error {
	_, err := call(ctx, d.s, opHold, holdRequest{ID: id, Until: until.UTC()})
	return err
}

func (d remoteDriver) Probe(ctx context.Context) error {
	_, err := call(ctx, d.s, opProbe, none{})
	return err
}

func (d remoteDriver) Close() error { return d.s.Close() }

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
