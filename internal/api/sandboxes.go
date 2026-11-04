package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

type Sandbox struct {
	ID              string     `json:"id"`
	PoolID          string     `json:"pool_id"`
	PoolLabel       string     `json:"pool_label" doc:"Empty if the pool is deleted."`
	ServerID        string     `json:"server_id"`
	ServerName      string     `json:"server_name"`
	State           string     `json:"state" enum:"starting,idle,busy,stopping,gone,failed"`
	RunnerName      string     `json:"runner_name"`
	IP              string     `json:"ip"`
	JobID           string     `json:"job_id" doc:"Empty until the runner gets a job."`
	JobName         string     `json:"job_name"`
	JobRepository   string     `json:"job_repository" doc:"owner/name of the job."`
	JobWorkflowFile string     `json:"job_workflow_file" doc:"Workflow file of the job, for example \".github/workflows/ci.yml\"."`
	Debug           bool       `json:"debug" doc:"The sandbox stays after its job for a debug shell."`
	HoldUntil       *time.Time `json:"hold_until" doc:"A debug sandbox stays until this time."`
	VCPU            int        `json:"vcpu"`
	MemoryMB        int        `json:"memory_mb"`
	Error           string     `json:"error" doc:"Why a failed sandbox failed."`
	CreatedAt       time.Time  `json:"created_at"`
	IdleAt          *time.Time `json:"idle_at"`
	BusyAt          *time.Time `json:"busy_at"`
	StoppedAt       *time.Time `json:"stopped_at"`
}

func (h *Handler) sandboxes(ctx context.Context, list []store.Sandbox, r refs) ([]Sandbox, error) {
	jobs := map[string]store.Job{}
	out := make([]Sandbox, len(list))
	for i, b := range list {
		j, ok := jobs[b.JobID]
		if !ok && b.JobID != "" {
			var err error
			if j, err = h.d.Store.GetJob(ctx, b.JobID); err != nil && !errors.Is(err, store.ErrNotFound) {
				return nil, err
			}
			jobs[b.JobID] = j
		}
		out[i] = Sandbox{
			ID: b.ID, PoolID: b.PoolID, PoolLabel: r.poolLabel(b.PoolID), ServerID: b.ServerID,
			ServerName: r.serverName(b.ServerID), State: string(b.State), RunnerName: b.RunnerName, IP: b.IP,
			JobID: b.JobID, JobName: j.JobName, JobRepository: j.Repository, JobWorkflowFile: j.WorkflowFile,
			Debug: b.Debug, HoldUntil: b.HoldUntil, VCPU: b.VCPU, MemoryMB: b.MemoryMB,
			Error: b.Error, CreatedAt: b.CreatedAt, IdleAt: b.IdleAt, BusyAt: b.BusyAt, StoppedAt: b.StoppedAt,
		}
	}
	return out, nil
}

type listSandboxesInput struct {
	PoolID   string `query:"pool_id" maxLength:"100"`
	ServerID string `query:"server_id" maxLength:"100"`
	State    string `query:"state" enum:"starting,idle,busy,stopping,gone,failed" doc:"Absent: the sandboxes that are not gone or failed. The list has at most 500 sandboxes, newest first."`
}

// maxSandboxes limits the list of gone and failed sandboxes, which grows
// until the retention cleanup.
const maxSandboxes = 500

func (h *Handler) listSandboxes(ctx context.Context, in *listSandboxesInput) (*struct{ Body []Sandbox }, error) {
	f := store.SandboxFilter{PoolID: in.PoolID, ServerID: in.ServerID}
	if in.State != "" {
		f.States = []sandbox.State{sandbox.State(in.State)}
	} else {
		f.States = []sandbox.State{sandbox.StateStarting, sandbox.StateIdle, sandbox.StateBusy, sandbox.StateStopping}
	}
	list, err := h.d.Store.ListSandboxes(ctx, f)
	if err != nil {
		return nil, h.internal(err)
	}
	r, err := h.loadRefs(ctx)
	if err != nil {
		return nil, h.internal(err)
	}
	out, err := h.sandboxes(ctx, list[:min(len(list), maxSandboxes)], r)
	if err != nil {
		return nil, h.internal(err)
	}
	return &struct{ Body []Sandbox }{out}, nil
}

func (h *Handler) getSandbox(ctx context.Context, in *PathID) (*struct{ Body Sandbox }, error) {
	b, err := h.d.Store.GetSandbox(ctx, in.ID)
	if err != nil {
		return nil, h.storeErr(err, "No sandbox has this ID.")
	}
	r, err := h.loadRefs(ctx)
	if err != nil {
		return nil, h.internal(err)
	}
	out, err := h.sandboxes(ctx, []store.Sandbox{b}, r)
	if err != nil {
		return nil, h.internal(err)
	}
	return &struct{ Body Sandbox }{out[0]}, nil
}

func (h *Handler) stopSandbox(ctx context.Context, in *PathID) (*empty, error) {
	if h.d.Scheduler == nil {
		return nil, unavailable("The scheduler")
	}
	b, err := h.d.Store.GetSandbox(ctx, in.ID)
	if err != nil {
		return nil, h.storeErr(err, "No sandbox has this ID.")
	}
	if b.State.Done() {
		return nil, nil
	}
	if err := h.d.Scheduler.StopSandbox(ctx, b.ID); err != nil {
		return nil, h.serviceErr(err)
	}
	h.audit(ctx, "sandbox.stop", b.ID, h.sandboxDetail(ctx, b))
	return nil, nil
}

// sandboxDetail names a sandbox and its job in an audit entry.
func (h *Handler) sandboxDetail(ctx context.Context, b store.Sandbox) map[string]any {
	detail := map[string]any{"runner_name": b.RunnerName}
	if b.JobID == "" {
		return detail
	}
	detail["job_id"] = b.JobID
	j, err := h.d.Store.GetJob(ctx, b.JobID)
	if err != nil {
		h.d.Log.Warn("read the job of a sandbox for the audit log", "sandbox", b.ID, "err", err)
		return detail
	}
	return jobDetail(detail, j)
}

type sandboxLogInput struct {
	PathID
	Follow bool `query:"follow" doc:"Stream new lines until the sandbox stops."`
}

func (h *Handler) sandboxLog(ctx context.Context, in *sandboxLogInput) (*huma.StreamResponse, error) {
	if h.d.Logs == nil {
		return nil, unavailable("The sandbox log")
	}
	if _, err := h.d.Store.GetSandbox(ctx, in.ID); err != nil {
		return nil, h.storeErr(err, "No sandbox has this ID.")
	}
	rc, err := h.d.Logs.SandboxLog(ctx, in.ID, in.Follow)
	if err != nil {
		return nil, h.serviceErr(err)
	}
	return h.stream(rc, "text/plain; charset=utf-8", in.Follow), nil
}

type shellInput struct {
	PathID
	Rows uint16 `query:"rows" minimum:"1" maximum:"500" default:"24"`
	Cols uint16 `query:"cols" minimum:"1" maximum:"1000" default:"80"`
}

// shellMessage is a text frame from the browser.
type shellMessage struct {
	Type string `json:"type"`
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

func (h *Handler) openShell(ctx context.Context, in *shellInput) (*huma.StreamResponse, error) {
	if h.d.Shell == nil {
		return nil, unavailable("The debug shell")
	}
	b, err := h.d.Store.GetSandbox(ctx, in.ID)
	if err != nil {
		return nil, h.storeErr(err, "No sandbox has this ID.")
	}
	if err := shellAllowed(b, time.Now()); err != nil {
		return nil, err
	}
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		r, w := humago.Unwrap(hctx)
		opts := &websocket.AcceptOptions{}
		if h.d.Config.Dev {
			opts.OriginPatterns = []string{"localhost:*", "127.0.0.1:*"}
		}
		// Accept checks that the Origin is this host, so a page on another
		// site cannot open a shell with the session cookie.
		conn, err := websocket.Accept(w, r, opts)
		if err != nil {
			return // Accept wrote the response
		}
		defer conn.CloseNow()
		conn.SetReadLimit(1 << 20)

		ended := make(chan struct{})
		defer close(ended)
		go func() {
			select {
			case <-h.done:
				conn.Close(websocket.StatusGoingAway, "runplane stops.")
			case <-ended:
			}
		}()

		ctx := r.Context()
		term, err := h.d.Shell.Open(ctx, b.ID, sandbox.TermSize{Rows: in.Rows, Cols: in.Cols})
		if err != nil {
			h.d.Log.Warn("open debug shell", "sandbox", b.ID, "err", err)
			conn.Close(websocket.StatusInternalError, closeReason(err.Error()))
			return
		}
		h.audit(ctx, "sandbox.shell", b.ID, h.sandboxDetail(ctx, b))
		bridge(ctx, conn, term)
	}}, nil
}

// shellAllowed applies the rule of the SSH gateway: only a debug sandbox
// has a shell, while its job runs or during its hold after the job.
func shellAllowed(b store.Sandbox, now time.Time) error {
	switch {
	case !b.Debug:
		return huma.Error403Forbidden("Only a debug sandbox has a shell. Re-run the job with debug to get one.")
	case b.State.Done():
		return huma.Error409Conflict("The sandbox has stopped.")
	case b.State == sandbox.StateBusy || b.HoldUntil != nil && now.Before(*b.HoldUntil):
		return nil
	case b.HoldUntil != nil:
		return huma.Error409Conflict("The debug hold of the sandbox has ended.")
	}
	return huma.Error409Conflict("The sandbox has no job yet. Open the shell when its job runs.")
}

// sshPort is the port of the SSH gateway from RUNPLANE_SSH_LISTEN, or the
// default 2222 when the config has no address.
func (h *Handler) sshPort() int {
	_, port, _ := net.SplitHostPort(h.d.Config.SSHListen)
	if n, err := strconv.Atoi(port); err == nil {
		return n
	}
	return 2222
}

// sshCommand is the command that opens the debug shell of a sandbox
// through the SSH gateway.
func (h *Handler) sshCommand(ctx context.Context, sandboxID string) (string, error) {
	base, err := h.baseURL(ctx)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("parse base URL: %w", err)
	}
	return fmt.Sprintf("ssh -p %d %s@%s", h.sshPort(), sandboxID, u.Hostname()), nil
}

// bridge copies terminal bytes between the WebSocket and term. It returns
// when the WebSocket closes. When the shell ends, it closes the WebSocket.
func bridge(ctx context.Context, conn *websocket.Conn, term sandbox.Terminal) {
	defer term.Close()
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := term.Read(buf)
			if n > 0 && conn.Write(ctx, websocket.MessageBinary, buf[:n]) != nil {
				return
			}
			if err != nil {
				conn.Close(websocket.StatusNormalClosure, "The shell ended.")
				return
			}
		}
	}()
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageBinary {
			if _, err := term.Write(data); err != nil {
				return
			}
			continue
		}
		var msg shellMessage
		if json.Unmarshal(data, &msg) == nil && msg.Type == "resize" && msg.Rows > 0 && msg.Cols > 0 {
			term.Resize(sandbox.TermSize{Rows: msg.Rows, Cols: msg.Cols})
		}
	}
}

// closeReason fits a WebSocket close reason, which has at most 123 bytes.
func closeReason(msg string) string {
	if len(msg) <= 120 {
		return msg
	}
	return msg[:120] + "..."
}
