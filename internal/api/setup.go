package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/runplane/runplane/internal/auth"
	"github.com/runplane/runplane/internal/store"
	"github.com/runplane/runplane/internal/version"
)

type Status struct {
	NeedsSetup      bool   `json:"needs_setup" doc:"True until the first admin exists."`
	Version         string `json:"version" doc:"Version of runplane, for example \"v1.2.0 (3f2a9c1)\"."`
	Mode            string `json:"mode" enum:"container,vm,dev," doc:"Sandbox mode. Empty until the install sets it."`
	InstanceName    string `json:"instance_name"`
	GitHubConnected bool   `json:"github_connected" doc:"True when a GitHub App with at least one installation exists."`
	HasJobs         bool   `json:"has_jobs" doc:"True when runplane has recorded at least one job."`
	SSHPort         int    `json:"ssh_port" doc:"Port of the SSH gateway for debug shells."`
}

func (h *Handler) getStatus(ctx context.Context, _ *struct{}) (*struct{ Body Status }, error) {
	st := h.d.Store
	users, err := st.CountUsers(ctx)
	if err != nil {
		return nil, h.internal(err)
	}
	settings, err := st.GetSettings(ctx)
	if err != nil {
		return nil, h.internal(err)
	}
	connected, err := h.githubConnected(ctx)
	if err != nil {
		return nil, h.internal(err)
	}
	jobs, _, err := st.ListJobs(ctx, store.JobFilter{Limit: 1})
	if err != nil {
		return nil, h.internal(err)
	}
	return &struct{ Body Status }{Status{
		NeedsSetup:      users == 0,
		Version:         version.String(),
		Mode:            string(h.mode(settings)),
		InstanceName:    settings.InstanceName,
		GitHubConnected: connected,
		HasJobs:         len(jobs) > 0,
		SSHPort:         h.sshPort(),
	}}, nil
}

func (h *Handler) githubConnected(ctx context.Context) (bool, error) {
	if _, err := h.d.Store.GetGitHubApp(ctx); errors.Is(err, store.ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	list, err := h.d.Store.ListInstallations(ctx)
	return len(list) > 0, err
}

type SetupRequest struct {
	Code     string `json:"code" doc:"Setup code from the install script, for example ABCD-EFGH-JKLM."`
	Name     string `json:"name" maxLength:"100"`
	Email    string `json:"email" maxLength:"254"`
	Password string `json:"password" maxLength:"1024"`
}

func (h *Handler) setup(ctx context.Context, in *struct{ Body SetupRequest }) (*sessionOutput, error) {
	c := clientOf(ctx)
	b := in.Body
	u, token, err := h.d.Auth.Setup(ctx, b.Code, b.Name, b.Email, b.Password, c.ip, c.userAgent)
	if err != nil {
		return nil, h.authErr(err, "body.password")
	}
	h.audit(context.WithValue(ctx, userKey{}, u), "setup.done", u.ID, map[string]any{"email": u.Email})
	return h.newSession(ctx, u, token), nil
}

type HostCheck struct {
	Hostname      string  `json:"hostname"`
	Mode          string  `json:"mode" enum:"container,vm,dev" doc:"Mode for this server: vm with KVM, container without, dev on a development machine."`
	KVM           bool    `json:"kvm"`
	Arch          string  `json:"arch" enum:"x64,arm64"`
	VCPU          int     `json:"vcpu"`
	MemoryMB      int     `json:"memory_mb"`
	DiskBytes     int64   `json:"disk_bytes" doc:"Size of the disk of the data folder."`
	FreeDiskBytes int64   `json:"free_disk_bytes" doc:"Free space on the disk of the data folder."`
	DiskKind      string  `json:"disk_kind" doc:"NVMe, SSD, or HDD. Empty if unknown."`
	OS            string  `json:"os" doc:"For example \"Ubuntu 24.04.1 LTS\"."`
	Checks        []Check `json:"checks"`
}

// Check is the result of one doctor check.
type Check struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Status  string `json:"status" enum:"ok,warn,fail"`
	Detail  string `json:"detail"`
	Fix     string `json:"fix" doc:"One sentence that says how to fix the problem when the status is not ok."`
	Command string `json:"command" doc:"Shell command that does the fix, if one does. Empty otherwise."`
}

type setupHostInput struct {
	Code string `query:"code" required:"true" doc:"The setup code."`
}

func (h *Handler) getSetupHost(ctx context.Context, in *setupHostInput) (*struct{ Body HostCheck }, error) {
	users, err := h.d.Store.CountUsers(ctx)
	if err != nil {
		return nil, h.internal(err)
	}
	if users > 0 {
		return nil, huma.Error409Conflict("The setup is done.")
	}
	ok, err := h.setupCodeValid(ctx, in.Code)
	if err != nil {
		return nil, h.internal(err)
	}
	if !ok {
		return nil, invalid("query.code", sentence(auth.ErrBadSetupCode), nil)
	}
	if h.d.HostChecker == nil {
		return nil, unavailable("The host check")
	}
	hc, err := h.d.HostChecker.Check(ctx)
	if err != nil {
		return nil, h.serviceErr(err)
	}
	hc.Checks = orEmpty(hc.Checks)
	// Server detail shows these checks as the last doctor result of the primary.
	if local, err := h.d.Store.GetLocalServer(ctx); err == nil {
		if _, err := h.saveDoctor(ctx, local.ID, hc.Checks); err != nil {
			h.d.Log.Warn("save doctor result", "server", local.ID, "err", err)
		}
	}
	return &struct{ Body HostCheck }{hc}, nil
}

// setupCodeValid reports whether code is an unused setup code that has not
// expired. auth has no method that checks a code without using it, so this
// repeats its hash: SHA-256 in hex of the code in uppercase without dashes
// and spaces.
func (h *Handler) setupCodeValid(ctx context.Context, code string) (bool, error) {
	code = strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(code)))
	sum := sha256.Sum256([]byte(code))
	var n int
	err := h.d.Store.DB().QueryRowContext(ctx,
		"SELECT count(*) FROM setup_codes WHERE code_hash = ? AND used_at IS NULL AND expires_at > ?",
		hex.EncodeToString(sum[:]), time.Now().UnixMilli()).Scan(&n)
	return n > 0, err
}
