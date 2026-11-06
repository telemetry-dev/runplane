package api

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"github.com/runplane/runplane"
	"github.com/runplane/runplane/internal/auth"
	"github.com/runplane/runplane/internal/field"
	"github.com/runplane/runplane/internal/store"
	"github.com/runplane/runplane/internal/version"
)

type Status struct {
	NeedsSetup      bool     `json:"needs_setup" doc:"True until the first admin exists."`
	Version         string   `json:"version" doc:"Version of runplane, for example \"v1.2.0 (3f2a9c1)\"."`
	Mode            string   `json:"mode" enum:"container,vm,dev" doc:"Sandbox mode."`
	InstanceName    string   `json:"instance_name"`
	GitHubConnected bool     `json:"github_connected" doc:"True when a GitHub App with at least one installation exists."`
	HasJobs         bool     `json:"has_jobs" doc:"True when runplane has recorded at least one job."`
	SSHPort         int      `json:"ssh_port" doc:"Port of the SSH gateway for debug shells."`
	Systems         []System `json:"systems" doc:"The operating systems of a server that install.sh names."`
}

type System struct {
	Name       string `json:"name" doc:"For example Ubuntu."`
	Version    string `json:"version" doc:"For example 24.04."`
	BestEffort bool   `json:"best_effort" doc:"install.sh installs runplane with a warning, because runplane has no tests on the system."`
}

func (h *Handler) getStatus(ctx context.Context, _ *struct{}) (*body[Status], error) {
	st := h.d.Store
	users, err := st.CountUsers(ctx)
	if err != nil {
		return nil, err
	}
	settings, err := st.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	connected, err := h.githubConnected(ctx)
	if err != nil {
		return nil, err
	}
	jobs, _, err := st.ListJobs(ctx, store.JobFilter{Limit: 1})
	if err != nil {
		return nil, err
	}
	systems := make([]System, len(runplane.Systems))
	for i, s := range runplane.Systems {
		systems[i] = System{Name: s.Name, Version: s.Version, BestEffort: s.BestEffort}
	}
	return &body[Status]{Status{
		NeedsSetup:      users == 0,
		Version:         version.String(),
		Mode:            string(h.d.Settings.Mode(settings)),
		InstanceName:    settings.InstanceName,
		GitHubConnected: connected,
		HasJobs:         len(jobs) > 0,
		SSHPort:         h.sshPort(),
		Systems:         systems,
	}}, nil
}

type SetupRequest struct {
	Code     string `json:"code" pattern:"^[A-HJ-NP-Za-hj-np-z2-9]{4}[- ]?[A-HJ-NP-Za-hj-np-z2-9]{4}[- ]?[A-HJ-NP-Za-hj-np-z2-9]{4}$" patternDescription:"a setup code such as ABCD-EFGH-JKLM" doc:"Setup code from the install script, for example ABCD-EFGH-JKLM."`
	Name     string `json:"name" maxLength:"100"`
	Email    string `json:"email" maxLength:"254"`
	Password string `json:"password" minLength:"10" maxLength:"1024"`
}

func (h *Handler) setup(ctx context.Context, in *body[SetupRequest]) (*sessionOutput, error) {
	c := clientOf(ctx)
	b := in.Body
	u, token, err := h.d.Auth.Setup(ctx, b.Code, b.Name, b.Email, b.Password, c.ip, c.userAgent)
	if err != nil {
		return nil, authErr(err, "body.password")
	}
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

type Check struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Status  string `json:"status" enum:"ok,warn,fail"`
	Detail  string `json:"detail"`
	Fix     string `json:"fix" doc:"One sentence that says how to fix the problem when the status is not ok."`
	Command string `json:"command" doc:"Shell command that does the fix, if one does. Empty otherwise."`
}

type setupHostInput struct {
	Code string `query:"code" required:"true" pattern:"^[A-HJ-NP-Za-hj-np-z2-9]{4}[- ]?[A-HJ-NP-Za-hj-np-z2-9]{4}[- ]?[A-HJ-NP-Za-hj-np-z2-9]{4}$" patternDescription:"a setup code such as ABCD-EFGH-JKLM" doc:"The setup code."`
}

func (h *Handler) getSetupHost(ctx context.Context, in *setupHostInput) (*body[HostCheck], error) {
	users, err := h.d.Store.CountUsers(ctx)
	if err != nil {
		return nil, err
	}
	if users > 0 {
		return nil, huma.Error409Conflict("The setup is done.")
	}
	ok, err := h.d.Auth.SetupCodeValid(ctx, in.Code)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, invalid("query.code", field.Sentence(auth.ErrBadSetupCode), nil)
	}
	c, err := h.d.HostChecker.Check(ctx)
	if err != nil {
		return nil, serviceErr(err)
	}
	i := c.Info
	hc := HostCheck{
		Hostname: i.Hostname, Mode: string(c.Mode), KVM: i.KVM, Arch: i.Arch, VCPU: i.VCPU, MemoryMB: i.MemoryMB,
		DiskBytes: i.DiskBytes, FreeDiskBytes: i.FreeDiskBytes, DiskKind: i.DiskKind, OS: i.OS, Checks: checks(c.Checks),
	}
	// Server detail shows these checks as the last doctor result of the primary.
	if local, err := h.d.Store.GetLocalServer(ctx); err == nil {
		if _, err := h.saveDoctor(ctx, local.ID, hc.Checks); err != nil {
			h.d.Log.Warn("save doctor result", "server", local.ID, "err", err)
		}
	}
	return &body[HostCheck]{hc}, nil
}
