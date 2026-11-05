package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/image"
	"github.com/runplane/runplane/internal/sandbox"
)

// Hello is the first line that an agent sends on a new connection. A new
// agent sends its install token, a known agent its server ID and secret.
type Hello struct {
	Token    string       `json:"token"`
	ServerID string       `json:"server_id"`
	Secret   string       `json:"secret"`
	Version  string       `json:"version"`
	Mode     sandbox.Mode `json:"mode"`
	Host     host.Info    `json:"host"`
	// Addr is the IP address of the agent. The Listener sets it.
	Addr string `json:"-"`
}

// reply is the line that the primary sends back.
type reply struct {
	OK       bool   `json:"ok"`
	ServerID string `json:"server_id,omitempty"`
	Secret   string `json:"secret,omitempty"`
	Error    string `json:"error,omitempty"`
}

// hostReport is the body of GET /v1/host. Doctor is set only with doctor=1.
type hostReport struct {
	Info   host.Info    `json:"info"`
	Doctor []host.Check `json:"doctor,omitempty"`
}

// imageMessage is one line of the POST /v1/images/ensure stream. The last
// line has Image or Error.
type imageMessage struct {
	Progress *image.Progress   `json:"progress,omitempty"`
	Image    *sandbox.ImageRef `json:"image,omitempty"`
	Error    string            `json:"error,omitempty"`
}

// streamItem is one line of the POST /v1/events stream.
type streamItem struct {
	Event *sandbox.Event `json:"event,omitempty"`
	Stats *host.Stats    `json:"stats,omitempty"`
}

type runnerRequest struct {
	Version string `json:"version"`
	Arch    string `json:"arch"`
}

type holdRequest struct {
	Until time.Time `json:"until"`
}

type updateRequest struct {
	Version string `json:"version"`
}

// gcRequest is the body of POST /v1/images/gc: the digests of the images
// that the server keeps.
type gcRequest struct {
	Keep []string `json:"keep"`
}

const (
	handshakeTimeout = 15 * time.Second
	maxLine          = 64 << 10 // hello and reply
	maxBody          = 1 << 20  // JSON request bodies
)

func yamuxConfig(log *slog.Logger) *yamux.Config {
	cfg := yamux.DefaultConfig()
	// Find a dead connection well within the 60 seconds after which the
	// primary marks an agent unreachable.
	cfg.KeepAliveInterval = 10 * time.Second
	// Cache blobs of several GB go through the tunnel. The default window
	// of 256 KiB limits a stream to 5 MB/s at a round trip of 50 ms.
	cfg.MaxStreamWindowSize = 8 << 20
	cfg.LogOutput = nil
	cfg.Logger = slog.NewLogLogger(log.Handler(), slog.LevelDebug)
	return cfg
}

// streamClient sends each HTTP request on a stream of sess.
func streamClient(sess *yamux.Session) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext:         func(context.Context, string, string) (net.Conn, error) { return sess.Open() },
		DisableCompression:  true,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     time.Minute,
	}}
}

// serveStreams serves HTTP on the streams that the other side opens, until
// the session ends.
func serveStreams(sess *yamux.Session, h http.Handler, log *slog.Logger) {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
	srv.Serve(sess)
}

// bufConn reads through the reader that read the handshake line, so that
// yamux gets the bytes that the reader already holds.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func readJSONLine(r *bufio.Reader, v any) error {
	line, err := r.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return errors.New("line is too long")
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(line, v)
}

func writeJSONLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// responseError reads the error text of a failed response.
func responseError(resp *http.Response) error {
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if text := strings.TrimSpace(string(msg)); text != "" {
		return errors.New(text)
	}
	return fmt.Errorf("status %s", resp.Status)
}

func remoteIP(addr net.Addr) string {
	ip, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return ip
}
