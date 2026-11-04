package main

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/runplane/runplane/internal/sandbox/control"
)

// dialHost connects to the host end of the control channel in container and
// dev mode. In dev mode the ctl folder also holds control.addr: then the
// guest dials TCP and sends the hello line first. It reads the file at each
// dial, because the host writes a new token after a restart.
func dialHost(ctx context.Context, socket string) (net.Conn, error) {
	// A connect that hangs, for example in the TCP proxy of Docker on
	// macOS, would otherwise wait for the kernel limit of about 2 minutes.
	d := net.Dialer{Timeout: 5 * time.Second}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(socket), control.AddrFile))
	if errors.Is(err, fs.ErrNotExist) {
		return d.DialContext(ctx, "unix", socket)
	}
	if err != nil {
		return nil, err
	}
	addr, hello, err := control.ParseAddr(b)
	if err != nil {
		return nil, err
	}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, hello); err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetWriteDeadline(time.Time{})
	return conn, nil
}
