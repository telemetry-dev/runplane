package api

import (
	"context"
	"errors"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/runplane/runplane/internal/doctor"
	"github.com/runplane/runplane/internal/field"
	"github.com/runplane/runplane/internal/fleet"
	"github.com/runplane/runplane/internal/policy"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

// ServerSummary is the server card of the overview.
type ServerSummary struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Kind          string     `json:"kind" enum:"local,agent" doc:"local is the primary."`
	Status        string     `json:"status" enum:"online,installing,unreachable,draining,disabled"`
	StatusDetail  string     `json:"status_detail" doc:"For example the install progress or the last error."`
	Arch          string     `json:"arch" enum:"x64,arm64," doc:"Empty until the server reports its facts."`
	Mode          string     `json:"mode" enum:"container,vm,dev," doc:"Empty until the server reports its facts."`
	VCPU          int        `json:"vcpu"`
	MemoryMB      int        `json:"memory_mb"`
	DiskBytes     int64      `json:"disk_bytes"`
	CPUPercent    *float64   `json:"cpu_percent" doc:"Latest host sample. Null without samples."`
	MemoryPercent *float64   `json:"memory_percent"`
	DiskPercent   *float64   `json:"disk_percent"`
	StatsAt       *time.Time `json:"stats_at" doc:"Time of the latest host sample."`
	RunningJobs   int        `json:"running_jobs" doc:"Sandboxes that run a job."`
	Sandboxes     int        `json:"sandboxes" doc:"Sandboxes that are not gone or failed."`
	VCPUInUse     int        `json:"vcpu_in_use" doc:"vCPUs of the live sandboxes."`
	MemoryInUseMB int        `json:"memory_in_use_mb"`
	LastSeenAt    *time.Time `json:"last_seen_at" doc:"Last message from an agent. Null for the primary."`
}

type Server struct {
	ServerSummary
	Address            string        `json:"address"`
	Provider           string        `json:"provider" doc:"Cloud provider, if runplane knows it."`
	Region             string        `json:"region" doc:"Region or location of the cloud provider, for example fsn1. Empty if unknown."`
	OS                 string        `json:"os"`
	Kernel             string        `json:"kernel"`
	DiskKind           string        `json:"disk_kind" doc:"NVMe, SSD, HDD, or empty if unknown."`
	KVM                bool          `json:"kvm"`
	DockerVersion      string        `json:"docker_version"`
	SysboxVersion      string        `json:"sysbox_version"`
	FirecrackerVersion string        `json:"firecracker_version"`
	Version            string        `json:"version" doc:"runplane version on the server."`
	UpdateState        string        `json:"update_state" enum:"current,available,updating,failed,none" doc:"current: the server runs the version of the primary. available: POST /api/servers/{id}/update installs the release of the primary on the agent. updating: an update runs. failed: the last update failed, and update_error tells why; POST again tries again. none: runplane cannot update the server, because the primary is not a release build or the server has not reported its version."`
	UpdateError        string        `json:"update_error" doc:"Why the last update of the agent failed. Empty when it did not fail."`
	WaitReason         string        `json:"wait_reason" doc:"Why the server gets no runners in its mode, for example while allow_public_repos is on and the server is not in VM mode. Empty when it gets runners. status tells if the server is draining or disabled."`
	Actions            ServerActions `json:"actions" doc:"The changes that an admin can make to the server now."`
	CreatedAt          time.Time     `json:"created_at"`
	UpdatedAt          time.Time     `json:"updated_at"`
}

type ServerActions struct {
	CanDrain     bool `json:"can_drain" doc:"PATCH status draining."`
	CanResume    bool `json:"can_resume" doc:"PATCH status online, after a drain or a disable."`
	CanRemove    bool `json:"can_remove" doc:"DELETE. The primary cannot be removed."`
	CanUpdate    bool `json:"can_update" doc:"POST /api/servers/{id}/update: the agent is connected, and update_state is available or failed."`
	CanRunDoctor bool `json:"can_run_doctor" doc:"POST /api/servers/{id}/doctor: the server is connected."`
}

// servers converts store servers and adds the latest host sample, the
// sandbox use, the update state, and why a server waits.
func (h *Handler) servers(ctx context.Context, list []store.Server) ([]Server, error) {
	stats, err := h.d.Store.LatestServerStats(ctx)
	if err != nil {
		return nil, err
	}
	live, err := h.d.Store.ListLiveSandboxes(ctx)
	if err != nil {
		return nil, err
	}
	settings, err := h.d.Store.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Server, len(list))
	byID := map[string]*Server{}
	for i, v := range list {
		f, a := v.ServerFacts, h.d.Servers.Actions(v)
		out[i] = Server{
			ServerSummary: ServerSummary{
				ID: v.ID, Name: v.Name, Kind: v.Kind, Status: v.Status, StatusDetail: v.StatusDetail, Arch: f.Arch,
				Mode: string(f.Mode), VCPU: f.VCPU, MemoryMB: f.MemoryMB, DiskBytes: f.DiskBytes, LastSeenAt: v.LastSeenAt,
			},
			Address: f.Address, Provider: f.Provider, Region: f.Region, OS: f.OS, Kernel: f.Kernel, DiskKind: f.DiskKind, KVM: f.KVM,
			DockerVersion: f.DockerVersion, SysboxVersion: f.SysboxVersion, FirecrackerVersion: f.FirecrackerVersion,
			Version: f.Version, UpdateState: string(h.d.Servers.UpdateState(v)), UpdateError: v.UpdateError,
			WaitReason: waitReason(settings, v),
			Actions: ServerActions{
				CanDrain: a.Drain, CanResume: a.Resume, CanRemove: a.Remove, CanUpdate: a.Update, CanRunDoctor: a.RunDoctor,
			},
			CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt,
		}
		if st, ok := stats[v.ID]; ok {
			out[i].CPUPercent, out[i].MemoryPercent, out[i].DiskPercent = &st.CPUPercent, &st.MemoryPercent, &st.DiskPercent
			out[i].StatsAt = &st.At
		}
		byID[v.ID] = &out[i]
	}
	for _, b := range live {
		v := byID[b.ServerID]
		if v == nil {
			continue
		}
		v.Sandboxes++
		v.VCPUInUse += b.VCPU
		v.MemoryInUseMB += b.MemoryMB
		if b.State == sandbox.StateBusy {
			v.RunningJobs++
		}
	}
	return out, nil
}

// waitReason tells why the scheduler starts no runners on the server, or
// returns "". A server without facts runs no driver yet. A public job that
// waits keeps a server out of its own scale set only, so it is no reason
// here.
func waitReason(st store.Settings, v store.Server) string {
	if v.Mode == "" {
		return ""
	}
	return policy.RunnerRefusal(st, v.Mode, false)
}

func (h *Handler) listServers(ctx context.Context, _ *struct{}) (*body[[]Server], error) {
	list, err := h.d.Store.ListServers(ctx)
	if err != nil {
		return nil, err
	}
	return reply(h.servers(ctx, list))
}

func (h *Handler) getServer(ctx context.Context, in *PathID) (*body[Server], error) {
	v, err := h.d.Store.GetServer(ctx, in.ID)
	if err != nil {
		return nil, notFound(err, "No server has this ID.")
	}
	return reply(first(h.servers(ctx, []store.Server{v})))
}

type ServerTokenRequest struct {
	Name string `json:"name,omitempty" maxLength:"64" doc:"Name of the new server. Empty: the host name of the server."`
}

type ServerToken struct {
	ID        string    `json:"token_id" doc:"Follow the install with GET /api/servers/tokens/{token_id}/status."`
	Token     string    `json:"token" doc:"One-time install token, rp_srv_..."`
	Command   string    `json:"command" doc:"Shell command that installs the agent on the new server."`
	ExpiresAt time.Time `json:"expires_at"`
}

func (h *Handler) createServerToken(ctx context.Context, in *body[*ServerTokenRequest]) (*body[ServerToken], error) {
	var name string
	if in.Body != nil {
		name = in.Body.Name
	}
	base, err := h.baseURL(ctx)
	if err != nil {
		return nil, err
	}
	t, err := h.d.Servers.NewToken(ctx, actor(ctx), base, name)
	if err != nil {
		return nil, serviceErr(err)
	}
	return &body[ServerToken]{ServerToken{ID: t.ID, Token: t.Token, Command: t.Command, ExpiresAt: t.ExpiresAt}}, nil
}

type ServerTokenStatus struct {
	Used      bool      `json:"used" doc:"An agent installed with the token."`
	ServerID  string    `json:"server_id" doc:"The server of the agent that used the token. Empty until then."`
	ExpiresAt time.Time `json:"expires_at" doc:"An unused token stops working at this time."`
}

func (h *Handler) getServerTokenStatus(ctx context.Context, in *PathID) (*body[ServerTokenStatus], error) {
	t, err := h.d.Store.GetServerToken(ctx, in.ID)
	if err != nil {
		return nil, notFound(err, "No install token has this ID.")
	}
	out := ServerTokenStatus{Used: t.UsedAt != nil, ExpiresAt: t.ExpiresAt}
	if out.Used {
		out.ServerID = t.ServerID
	}
	return &body[ServerTokenStatus]{out}, nil
}

type SSHInstall struct {
	Name string `json:"name" minLength:"1" maxLength:"64"`
	Host string `json:"host" minLength:"1" maxLength:"253" doc:"Host name or IP address of the new server."`
	Port int    `json:"port,omitempty" minimum:"1" maximum:"65535" default:"22"`
	User string `json:"user,omitempty" maxLength:"32" default:"root" doc:"A user with root or sudo without a password."`
}

func (h *Handler) installServerSSH(ctx context.Context, in *body[SSHInstall]) (*body[Server], error) {
	req := in.Body
	base, err := h.baseURL(ctx)
	if err != nil {
		return nil, err
	}
	id, err := h.d.Servers.InstallSSH(ctx, actor(ctx), base, fleet.SSHInstall{Name: req.Name, Host: req.Host, Port: req.Port, User: req.User})
	if err != nil {
		return nil, serviceErr(err)
	}
	v, err := h.d.Store.GetServer(ctx, id)
	if err != nil {
		return nil, notFound(err, "The new server is gone.")
	}
	return reply(first(h.servers(ctx, []store.Server{v})))
}

type SSHKey struct {
	PublicKey string `json:"public_key" doc:"Line for ~/.ssh/authorized_keys."`
}

func (h *Handler) getSSHKey(ctx context.Context, _ *struct{}) (*body[SSHKey], error) {
	key, err := h.d.Servers.SSHPublicKey(ctx)
	if err != nil {
		return nil, serviceErr(err)
	}
	return &body[SSHKey]{SSHKey{PublicKey: key}}, nil
}

// UpdateServerRequest has the name limits of SSHInstall.
type UpdateServerRequest struct {
	Name   *string `json:"name,omitempty"`
	Status *string `json:"status,omitempty" enum:"online,draining,disabled" doc:"draining and disabled servers get no new sandboxes. online makes a draining or disabled server take sandboxes again."`
}

func (UpdateServerRequest) TransformSchema(r huma.Registry, s *huma.Schema) *huma.Schema {
	return patchOf[SSHInstall](r, s)
}

func (h *Handler) updateServer(ctx context.Context, in *struct {
	PathID
	Body UpdateServerRequest
}) (*body[Server], error) {
	v, err := h.d.Servers.UpdateServer(ctx, actor(ctx), in.ID, fleet.ServerChange{Name: in.Body.Name, Status: in.Body.Status})
	if errors.Is(err, fleet.ErrInstalling) || errors.Is(err, fleet.ErrNotPaused) {
		return nil, huma.Error409Conflict(field.Sentence(err))
	}
	if err != nil {
		return nil, notFound(err, "No server has this ID.")
	}
	return reply(first(h.servers(ctx, []store.Server{v})))
}

func (h *Handler) deleteServer(ctx context.Context, in *PathID) (*empty, error) {
	err := h.d.Servers.RemoveServer(ctx, actor(ctx), in.ID)
	switch {
	case errors.Is(err, fleet.ErrLocalServer):
		return nil, huma.Error409Conflict("The primary server cannot be removed.")
	case errors.Is(err, store.ErrNotFound):
		return nil, huma.Error404NotFound("No server has this ID.")
	case err != nil:
		return nil, serviceErr(err)
	}
	return nil, nil
}

func (h *Handler) updateAgent(ctx context.Context, in *PathID) (*body[Server], error) {
	v, err := h.d.Store.GetServer(ctx, in.ID)
	if err != nil {
		return nil, notFound(err, "No server has this ID.")
	}
	switch err := h.d.Servers.UpdateAgent(ctx, actor(ctx), v.ID); {
	case errors.Is(err, fleet.ErrUpToDate):
		return nil, huma.Error409Conflict("The server runs runplane " + v.Version + ", the version of the primary.")
	case errors.Is(err, fleet.ErrUpdateInProgress):
		return nil, huma.Error409Conflict("An update of the server runs.")
	case errors.Is(err, fleet.ErrCannotUpdate):
		return nil, huma.Error409Conflict("runplane cannot update the server: the primary is not a release build, or the server has not reported its version.")
	case errors.Is(err, fleet.ErrNotConnected):
		return nil, huma.Error409Conflict("The server is not connected, so it cannot update.")
	case err != nil:
		return nil, serviceErr(err)
	}
	return reply(first(h.servers(ctx, []store.Server{v})))
}

type DoctorResult struct {
	ServerID  string     `json:"server_id"`
	CheckedAt *time.Time `json:"checked_at" doc:"Null when the doctor has not run on the server."`
	Checks    []Check    `json:"checks"`
}

func (h *Handler) getDoctor(ctx context.Context, in *PathID) (*body[DoctorResult], error) {
	v, err := h.d.Store.GetServer(ctx, in.ID)
	if err != nil {
		return nil, notFound(err, "No server has this ID.")
	}
	r, err := h.d.Store.GetDoctorResult(ctx, v.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return &body[DoctorResult]{DoctorResult{ServerID: v.ID, Checks: []Check{}}}, nil
	case err != nil:
		return nil, err
	}
	return &body[DoctorResult]{doctorResult(r)}, nil
}

func doctorResult(r store.DoctorResult) DoctorResult {
	out := DoctorResult{ServerID: r.ServerID, CheckedAt: &r.CheckedAt, Checks: []Check{}}
	for _, c := range r.Checks {
		out.Checks = append(out.Checks, Check(c))
	}
	return out
}

func (h *Handler) runDoctor(ctx context.Context, in *PathID) (*body[DoctorResult], error) {
	v, err := h.d.Store.GetServer(ctx, in.ID)
	if err != nil {
		return nil, notFound(err, "No server has this ID.")
	}
	r, err := h.d.Servers.Doctor(ctx, v.ID)
	if errors.Is(err, fleet.ErrNotConnected) {
		return nil, huma.Error409Conflict("The server is not connected, so its checks cannot run.")
	}
	if err != nil {
		return nil, serviceErr(err)
	}
	return &body[DoctorResult]{doctorResult(r)}, nil
}

func checks(list []doctor.Check) []Check {
	out := make([]Check, len(list))
	for i, c := range list {
		out[i] = Check{ID: c.ID, Name: c.Name, Status: string(c.Status), Detail: c.Detail, Fix: c.Fix, Command: c.Command}
	}
	return out
}
