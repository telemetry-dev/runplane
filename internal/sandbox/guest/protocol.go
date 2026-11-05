package guest

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/runplane/runplane/internal/sandbox"
)

// The control channel is one connection from the guest to the host, with
// yamux on it (YamuxConfig). Each side serves HTTP/1.1 on the streams that
// the other side opens. The host serves PathConfig, PathEvents, and
// PathLogs, and the guest serves the others. Each POST answers 204 No
// Content when it succeeds.
const (
	PathConfig = "/v1/config" // GET: 200 with the Config, or StatusNoConfig
	PathEvents = "/v1/events" // POST a sandbox.RunnerEvent
	// PathLogs takes the log stream (AppendLogLine) until the guest ends it.
	// The host answers only after the end.
	PathLogs     = "/v1/logs"
	PathHold     = "/v1/hold"     // POST a Hold
	PathShutdown = "/v1/shutdown" // POST: stop the runner and power off
	// PathShell opens a login shell as RunnerUser. The request asks for an
	// upgrade to ShellProtocol, with the terminal size in the query
	// (SizeQuery). The guest answers 101 Switching Protocols with the
	// session in SessionHeader, and the stream then carries the PTY bytes.
	PathShell = "/v1/shell"
	// PathShellResize changes the terminal size of the session in the
	// query QuerySession.
	PathShellResize = "/v1/shell/resize"
)

const (
	ShellProtocol = "runplane-shell"
	SessionHeader = "X-Runplane-Session"
	QuerySession  = "session"
)

// StatusNoConfig is the answer to PathConfig when the host has no config
// for the guest: see ErrNoConfig.
const StatusNoConfig = http.StatusConflict

// ErrNoConfig comes from Client.Config when runplane restarted before the
// guest got its config. The JIT config is only in the memory of the host, so
// the sandbox can never run a job, and the host stops it.
var ErrNoConfig = errors.New("runplane restarted before this sandbox got its config")

// ErrRejected comes from Client.PostEvent when the host answers with an
// error. The host does not take the event, also when it gets it again.
var ErrRejected = errors.New("the host rejected the event")

// Hold is the body of PathHold: the guest waits until Until after the
// runner exits.
type Hold struct {
	Until time.Time `json:"until"`
}

// SizeQuery is the query of a shell request for a terminal of size.
func SizeQuery(size sandbox.TermSize) url.Values {
	return url.Values{
		"rows": {strconv.Itoa(int(size.Rows))},
		"cols": {strconv.Itoa(int(size.Cols))},
	}
}

// ParseSize reads the terminal size of a shell request. A missing or zero
// value gives 24 rows and 80 columns.
func ParseSize(q url.Values) (sandbox.TermSize, error) {
	size := sandbox.TermSize{Rows: 24, Cols: 80}
	for name, v := range map[string]*uint16{"rows": &size.Rows, "cols": &size.Cols} {
		s := q.Get(name)
		if s == "" {
			continue
		}
		n, err := strconv.ParseUint(s, 10, 16)
		if err != nil {
			return size, fmt.Errorf("bad %s %q", name, s)
		}
		if n > 0 {
			*v = uint16(n)
		}
	}
	return size, nil
}

// YamuxConfig is the yamux setup of the two sides. The yamux messages go to
// log at the debug level.
func YamuxConfig(log *slog.Logger) *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = nil
	cfg.Logger = slog.NewLogLogger(log.Handler(), slog.LevelDebug)
	return cfg
}

// AddrFile in CtlDir sends the guest to TCP in dev mode, because a Unix
// socket in a bind mount does not cross the VM of Docker Desktop or
// OrbStack. Its one line is "tcp://<host:port> <sandbox-id> <token>". The
// guest then starts each connection with the hello line
// "RUNPLANE <sandbox-id> <token>".
const AddrFile = "control.addr"

// FormatAddr is the content of an AddrFile.
func FormatAddr(addr, id, token string) string {
	return "tcp://" + addr + " " + id + " " + token + "\n"
}

// ParseAddr reads the content of an AddrFile. It returns the TCP address to
// dial and the hello line to send first.
func ParseAddr(content []byte) (addr, hello string, err error) {
	f := strings.Fields(string(bytes.TrimSpace(content)))
	if len(f) != 3 || !strings.HasPrefix(f[0], "tcp://") {
		return "", "", fmt.Errorf("bad control address %q", content)
	}
	return strings.TrimPrefix(f[0], "tcp://"), "RUNPLANE " + f[1] + " " + f[2] + "\n", nil
}

// ParseHello reads a hello line without its line end.
func ParseHello(line string) (id, token string, err error) {
	f := strings.Fields(line)
	if len(f) != 3 || f[0] != "RUNPLANE" {
		return "", "", errors.New("bad hello line")
	}
	return f[1], f[2], nil
}
