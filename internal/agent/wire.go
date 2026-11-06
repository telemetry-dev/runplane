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

	"github.com/runplane/runplane/internal/doctor"
	"github.com/runplane/runplane/internal/host"
	"github.com/runplane/runplane/internal/image"
	"github.com/runplane/runplane/internal/mirror"
	"github.com/runplane/runplane/internal/runner"
	"github.com/runplane/runplane/internal/sandbox"
)

// Hello is the first message of an agent on a new connection. A new agent
// sends its install token, a known agent its server ID and secret.
type Hello struct {
	Token    string
	ServerID string
	Secret   string
	Version  string
	Mode     sandbox.Mode
	Host     host.Info
	// Addr is the IP address of the agent. The Listener sets it.
	Addr string
}

// The types below are the JSON of the protocol. They are apart from the
// types of the other packages, so that a change in those packages cannot
// change the protocol for agents of an older release. The sandbox types are
// the driver contract, and they go as they are.

// helloLine is the first line that an agent sends.
type helloLine struct {
	Token    string       `json:"token"`
	ServerID string       `json:"server_id"`
	Secret   string       `json:"secret"`
	Version  string       `json:"version"`
	Mode     sandbox.Mode `json:"mode"`
	Host     hostInfo     `json:"host"`
}

func (h helloLine) hello() Hello {
	return Hello{Token: h.Token, ServerID: h.ServerID, Secret: h.Secret, Version: h.Version, Mode: h.Mode, Host: h.Host.info()}
}

// reply is the line that the primary sends back.
type reply struct {
	OK       bool   `json:"ok"`
	ServerID string `json:"server_id,omitempty"`
	Secret   string `json:"secret,omitempty"`
	Error    string `json:"error,omitempty"`
}

// An agent of an earlier release sends more facts in hostInfo, which only its
// doctor read. The primary ignores them.
type hostInfo struct {
	Hostname           string `json:"hostname"`
	OS                 string `json:"os"`
	Kernel             string `json:"kernel"`
	Arch               string `json:"arch"`
	VCPU               int    `json:"vcpu"`
	MemoryMB           int    `json:"memory_mb"`
	DiskBytes          int64  `json:"disk_bytes"`
	FreeDiskBytes      int64  `json:"free_disk_bytes"`
	DiskKind           string `json:"disk_kind"`
	KVM                bool   `json:"kvm"`
	DockerVersion      string `json:"docker_version"`
	SysboxVersion      string `json:"sysbox_version"`
	FirecrackerVersion string `json:"firecracker_version"`
	IPv4               string `json:"ipv4"`
	Provider           string `json:"provider"`
	Region             string `json:"region"`
}

func wireInfo(i host.Info) hostInfo {
	return hostInfo{
		Hostname: i.Hostname, OS: i.OS, Kernel: i.Kernel, Arch: i.Arch,
		VCPU: i.VCPU, MemoryMB: i.MemoryMB, DiskBytes: i.DiskBytes, FreeDiskBytes: i.FreeDiskBytes,
		DiskKind: i.DiskKind, KVM: i.KVM, DockerVersion: i.DockerVersion, SysboxVersion: i.SysboxVersion,
		FirecrackerVersion: i.FirecrackerVersion, IPv4: i.IPv4, Provider: i.Provider, Region: i.Region,
	}
}

func (i hostInfo) info() host.Info {
	return host.Info{
		Hostname: i.Hostname, OS: i.OS, Kernel: i.Kernel, Arch: i.Arch,
		Resources: host.Resources{VCPU: i.VCPU, MemoryMB: i.MemoryMB, DiskBytes: i.DiskBytes, FreeDiskBytes: i.FreeDiskBytes},
		DiskKind:  i.DiskKind, KVM: i.KVM, DockerVersion: i.DockerVersion, SysboxVersion: i.SysboxVersion,
		FirecrackerVersion: i.FirecrackerVersion, IPv4: i.IPv4, Provider: i.Provider, Region: i.Region,
	}
}

type hostCheck struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	Detail  string `json:"detail"`
	Fix     string `json:"fix,omitempty"`
	Command string `json:"command,omitempty"`
}

func wireChecks(list []doctor.Check) []hostCheck {
	out := make([]hostCheck, len(list))
	for i, c := range list {
		out[i] = hostCheck{ID: c.ID, Name: c.Name, Status: string(c.Status), Detail: c.Detail, Fix: c.Fix, Command: c.Command}
	}
	return out
}

func checks(list []hostCheck) []doctor.Check {
	out := make([]doctor.Check, len(list))
	for i, c := range list {
		out[i] = doctor.Check{ID: c.ID, Name: c.Name, Status: doctor.Status(c.Status), Detail: c.Detail, Fix: c.Fix, Command: c.Command}
	}
	return out
}

type hostStats struct {
	At            time.Time `json:"at"`
	CPUPercent    float64   `json:"cpu_percent"`
	MemoryPercent float64   `json:"memory_percent"`
	DiskPercent   float64   `json:"disk_percent"`
	VCPU          int       `json:"vcpu"`
	MemoryMB      int       `json:"memory_mb"`
	DiskBytes     int64     `json:"disk_bytes"`
	FreeDiskBytes int64     `json:"free_disk_bytes"`
}

func wireStats(s host.Stats) hostStats {
	return hostStats{
		At: s.At, CPUPercent: s.CPUPercent, MemoryPercent: s.MemoryPercent, DiskPercent: s.DiskPercent,
		VCPU: s.VCPU, MemoryMB: s.MemoryMB, DiskBytes: s.DiskBytes, FreeDiskBytes: s.FreeDiskBytes,
	}
}

func (s hostStats) stats() host.Stats {
	return host.Stats{
		At: s.At, CPUPercent: s.CPUPercent, MemoryPercent: s.MemoryPercent, DiskPercent: s.DiskPercent,
		Resources: host.Resources{VCPU: s.VCPU, MemoryMB: s.MemoryMB, DiskBytes: s.DiskBytes, FreeDiskBytes: s.FreeDiskBytes},
	}
}

type progress struct {
	Status string `json:"status"`
	Done   int64  `json:"done"`
	Total  int64  `json:"total"`
	Error  string `json:"error,omitempty"`
}

// bundle is a runner bundle that is ready on the agent. Its folder is a
// path on the agent, so the primary does not get it.
type bundle struct {
	Version string `json:"version"`
	Arch    string `json:"arch"`
	SHA256  string `json:"sha256"`
	Patched bool   `json:"patched"`
}

type mirrorStats struct {
	Hits      int64 `json:"hits"`
	Misses    int64 `json:"misses"`
	HitBytes  int64 `json:"hit_bytes"`
	MissBytes int64 `json:"miss_bytes"`
	SizeBytes int64 `json:"size_bytes"`
}

func wireBundle(b runner.Bundle) bundle {
	return bundle{Version: b.Version, Arch: b.Arch, SHA256: b.SHA256, Patched: b.Patched}
}

func (b bundle) runnerBundle() runner.Bundle {
	return runner.Bundle{Version: b.Version, Arch: b.Arch, SHA256: b.SHA256, Patched: b.Patched}
}

func wireMirror(m mirror.Stats) mirrorStats {
	return mirrorStats{Hits: m.Hits, Misses: m.Misses, HitBytes: m.HitBytes, MissBytes: m.MissBytes, SizeBytes: m.SizeBytes}
}

func (m mirrorStats) stats() mirror.Stats {
	return mirror.Stats{Hits: m.Hits, Misses: m.Misses, HitBytes: m.HitBytes, MissBytes: m.MissBytes, SizeBytes: m.SizeBytes}
}

func wireProgress(p image.Progress) progress {
	return progress{Status: string(p.Status), Done: p.Done, Total: p.Total, Error: p.Error}
}

func (p progress) imageProgress() image.Progress {
	return image.Progress{Status: image.Status(p.Status), Done: p.Done, Total: p.Total, Error: p.Error}
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
