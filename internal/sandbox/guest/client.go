package guest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/hashicorp/yamux"

	"github.com/runplane/runplane/internal/sandbox"
)

// Client is the guest end of one control connection.
type Client struct {
	sess *yamux.Session
	http *http.Client
}

// NewClient starts the guest end on conn, a new connection to the host. It
// serves mux to the host. A failure closes conn.
func NewClient(conn net.Conn, mux http.Handler, log *slog.Logger) (*Client, error) {
	sess, err := yamux.Client(conn, YamuxConfig(log))
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("start yamux session: %w", err)
	}
	srv := &http.Server{Handler: mux, ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelDebug)}
	go srv.Serve(sess)
	return &Client{sess: sess, http: &http.Client{Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) { return sess.Open() },
	}}}, nil
}

// Done is closed when the connection drops.
func (c *Client) Done() <-chan struct{} { return c.sess.CloseChan() }

func (c *Client) Close() error { return c.sess.Close() }

// Config gets the job config. It returns ErrNoConfig when the host has none.
func (c *Client) Config(ctx context.Context) (Config, error) {
	var cfg Config
	resp, err := c.do(ctx, http.MethodGet, PathConfig, "", nil)
	if err != nil {
		return cfg, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return cfg, json.NewDecoder(resp.Body).Decode(&cfg)
	case StatusNoConfig:
		return cfg, ErrNoConfig
	}
	return cfg, statusError(resp)
}

// PostEvent sends a runner event.
func (c *Client) PostEvent(ctx context.Context, ev sandbox.RunnerEvent) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodPost, PathEvents, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("%w: %w", ErrRejected, statusError(resp))
	}
	return nil
}

// StreamLogs sends the log stream r in one request. It returns nil when the
// host has all of r.
func (c *Client) StreamLogs(ctx context.Context, r io.Reader) error {
	resp, err := c.do(ctx, http.MethodPost, PathLogs, "text/plain", r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return statusError(resp)
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, path, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://host"+path, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return c.http.Do(req)
}

func statusError(resp *http.Response) error {
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("host answered %s: %s", resp.Status, strings.TrimSpace(string(msg)))
}
