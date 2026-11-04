package api

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
	"github.com/runplane/runplane/internal/version"
)

// ServerSummary is the server card of the overview.
type ServerSummary struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Kind          string     `json:"kind" enum:"local,agent" doc:"local is the primary."`
	Status        string     `json:"status" enum:"online,installing,unreachable,draining,disabled"`
	StatusDetail  string     `json:"status_detail" doc:"For example the install progress or the last error."`
	Arch          string     `json:"arch" doc:"x64 or arm64. Empty until the server reports its facts."`
	Mode          string     `json:"mode" doc:"container, vm, or dev. Empty until the server reports its facts."`
	VCPU          int        `json:"vcpu"`
	MemoryMB      int        `json:"memory_mb"`
	DiskBytes     int64      `json:"disk_bytes"`
	CPUPercent    *float64   `json:"cpu_percent" doc:"Latest host sample. Null without samples."`
	MemoryPercent *float64   `json:"memory_percent"`
	DiskPercent   *float64   `json:"disk_percent"`
	RunningJobs   int        `json:"running_jobs" doc:"Sandboxes that run a job."`
	Sandboxes     int        `json:"sandboxes" doc:"Sandboxes that are not gone or failed."`
	VCPUInUse     int        `json:"vcpu_in_use" doc:"vCPUs of the live sandboxes."`
	MemoryInUseMB int        `json:"memory_in_use_mb"`
	LastSeenAt    *time.Time `json:"last_seen_at" doc:"Last message from an agent. Null for the primary."`
}

type Server struct {
	ServerSummary
	Address            string     `json:"address"`
	Provider           string     `json:"provider" doc:"Cloud provider, if runplane knows it."`
	Region             string     `json:"region" doc:"Region or location of the cloud provider, for example fsn1. Empty if unknown."`
	OS                 string     `json:"os"`
	Kernel             string     `json:"kernel"`
	DiskKind           string     `json:"disk_kind" doc:"For example nvme, ssd, or hdd."`
	KVM                bool       `json:"kvm"`
	DockerVersion      string     `json:"docker_version"`
	SysboxVersion      string     `json:"sysbox_version"`
	FirecrackerVersion string     `json:"firecracker_version"`
	Version            string     `json:"version" doc:"runplane version on the server."`
	UpdateState        string     `json:"update_state" enum:"current,available,updating,failed,none" doc:"current: the server runs the version of the primary. available: POST /api/servers/{id}/update installs the release of the primary on the agent. updating: an update runs. failed: the last update failed, and status_detail tells why; POST again tries again. none: runplane cannot update the server, because the primary is not a release build or the server has not reported its version."`
	StatsAt            *time.Time `json:"stats_at" doc:"Time of the latest host sample."`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

// Values of Server.UpdateState.
const (
	UpdateCurrent   = "current"
	UpdateAvailable = "available"
	UpdateUpdating  = "updating"
	UpdateFailed    = "failed"
	UpdateNone      = "none"
)

// servers converts store servers and adds the latest host sample, the
// sandbox use, and the update state.
func (h *Handler) servers(ctx context.Context, list []store.Server) ([]Server, error) {
	stats, err := h.d.Store.LatestServerStats(ctx)
	if err != nil {
		return nil, err
	}
	live, err := h.d.Store.ListLiveSandboxes(ctx)
	if err != nil {
		return nil, err
	}
	regions, err := h.d.Store.ServerRegions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Server, len(list))
	byID := map[string]*Server{}
	for i, v := range list {
		f := v.ServerFacts
		out[i] = Server{
			ServerSummary: ServerSummary{
				ID: v.ID, Name: v.Name, Kind: v.Kind, Status: v.Status, StatusDetail: v.StatusDetail, Arch: f.Arch,
				Mode: string(f.Mode), VCPU: f.VCPU, MemoryMB: f.MemoryMB, DiskBytes: f.DiskBytes, LastSeenAt: v.LastSeenAt,
			},
			Address: f.Address, Provider: f.Provider, Region: regions[v.ID], OS: f.OS, Kernel: f.Kernel, DiskKind: f.DiskKind, KVM: f.KVM,
			DockerVersion: f.DockerVersion, SysboxVersion: f.SysboxVersion, FirecrackerVersion: f.FirecrackerVersion,
			Version: f.Version, UpdateState: UpdateNone, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt,
		}
		if h.d.Servers != nil {
			out[i].UpdateState = h.d.Servers.UpdateState(v)
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

func (h *Handler) listServers(ctx context.Context, _ *struct{}) (*struct{ Body []Server }, error) {
	list, err := h.d.Store.ListServers(ctx)
	if err != nil {
		return nil, h.internal(err)
	}
	out, err := h.servers(ctx, list)
	if err != nil {
		return nil, h.internal(err)
	}
	return &struct{ Body []Server }{out}, nil
}

func (h *Handler) getServer(ctx context.Context, in *PathID) (*struct{ Body Server }, error) {
	v, err := h.d.Store.GetServer(ctx, in.ID)
	if err != nil {
		return nil, h.storeErr(err, "No server has this ID.")
	}
	return h.serverOutput(ctx, v)
}

func (h *Handler) serverOutput(ctx context.Context, v store.Server) (*struct{ Body Server }, error) {
	out, err := h.servers(ctx, []store.Server{v})
	if err != nil {
		return nil, h.internal(err)
	}
	return &struct{ Body Server }{out[0]}, nil
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

func (h *Handler) createServerToken(ctx context.Context, in *struct{ Body *ServerTokenRequest }) (*struct{ Body ServerToken }, error) {
	if h.d.Servers == nil {
		return nil, unavailable("Adding servers")
	}
	var name string
	if in.Body != nil {
		name = strings.TrimSpace(in.Body.Name)
	}
	base, err := h.baseURL(ctx)
	if err != nil {
		return nil, h.internal(err)
	}
	t, err := h.d.Servers.NewToken(ctx, base, name, currentUser(ctx).ID)
	if err != nil {
		return nil, h.serviceErr(err)
	}
	h.audit(ctx, "server.token", t.ID, map[string]any{"name": name, "expires_at": t.ExpiresAt})
	return &struct{ Body ServerToken }{t}, nil
}

type ServerTokenStatus struct {
	Used      bool      `json:"used" doc:"An agent installed with the token."`
	ServerID  string    `json:"server_id" doc:"The server of the agent that used the token. Empty until then."`
	ExpiresAt time.Time `json:"expires_at" doc:"An unused token stops working at this time."`
}

func (h *Handler) getServerTokenStatus(ctx context.Context, in *PathID) (*struct{ Body ServerTokenStatus }, error) {
	t, err := h.d.Store.GetServerToken(ctx, in.ID)
	if err != nil {
		return nil, h.storeErr(err, "No install token has this ID.")
	}
	out := ServerTokenStatus{Used: t.UsedAt != nil, ExpiresAt: t.ExpiresAt}
	if out.Used {
		out.ServerID = t.ServerID
	}
	return &struct{ Body ServerTokenStatus }{out}, nil
}

type SSHInstall struct {
	Name string `json:"name" minLength:"1" maxLength:"64"`
	Host string `json:"host" minLength:"1" maxLength:"253" doc:"Host name or IP address of the new server."`
	Port int    `json:"port,omitempty" minimum:"1" maximum:"65535" default:"22"`
	User string `json:"user,omitempty" maxLength:"32" default:"root" doc:"A user with root or sudo without a password."`
}

func (h *Handler) installServerSSH(ctx context.Context, in *struct{ Body SSHInstall }) (*struct{ Body Server }, error) {
	if h.d.Servers == nil {
		return nil, unavailable("Adding servers")
	}
	req := in.Body
	req.Name, req.Host = strings.TrimSpace(req.Name), strings.TrimSpace(req.Host)
	var errs problems
	if req.Name == "" {
		errs.add("body.name", "Enter a name.", in.Body.Name)
	}
	if req.Host == "" || strings.ContainsAny(req.Host, " /@") {
		errs.add("body.host", "Enter a host name or an IP address.", in.Body.Host)
	}
	if err := errs.err(); err != nil {
		return nil, err
	}
	base, err := h.baseURL(ctx)
	if err != nil {
		return nil, h.internal(err)
	}
	id, err := h.d.Servers.InstallSSH(ctx, base, req)
	if err != nil {
		return nil, h.serviceErr(err)
	}
	h.audit(ctx, "server.install", id, map[string]any{"name": req.Name, "host": req.Host, "port": req.Port, "user": req.User})
	v, err := h.d.Store.GetServer(ctx, id)
	if err != nil {
		return nil, h.storeErr(err, "The new server is gone.")
	}
	return h.serverOutput(ctx, v)
}

type SSHKey struct {
	PublicKey string `json:"public_key" doc:"Line for ~/.ssh/authorized_keys."`
}

func (h *Handler) getSSHKey(ctx context.Context, _ *struct{}) (*struct{ Body SSHKey }, error) {
	if h.d.Servers == nil {
		return nil, unavailable("Adding servers")
	}
	key, err := h.d.Servers.SSHPublicKey(ctx)
	if err != nil {
		return nil, h.serviceErr(err)
	}
	return &struct{ Body SSHKey }{SSHKey{PublicKey: key}}, nil
}

type UpdateServerRequest struct {
	Name   *string `json:"name,omitempty" minLength:"1" maxLength:"64"`
	Status *string `json:"status,omitempty" enum:"online,draining,disabled" doc:"draining and disabled servers get no new sandboxes. online makes a draining or disabled server take sandboxes again."`
}

func (h *Handler) updateServer(ctx context.Context, in *struct {
	PathID
	Body UpdateServerRequest
}) (*struct{ Body Server }, error) {
	v, err := h.d.Store.GetServer(ctx, in.ID)
	if err != nil {
		return nil, h.storeErr(err, "No server has this ID.")
	}
	b := in.Body
	var name string
	if b.Name != nil {
		if name = strings.TrimSpace(*b.Name); name == "" {
			return nil, invalid("body.name", "Enter a name.", *b.Name)
		}
	}
	changeStatus := b.Status != nil && *b.Status != v.Status
	if changeStatus {
		if v.Status == store.ServerInstalling {
			return nil, huma.Error409Conflict("The server is still installing.")
		}
		if *b.Status == store.ServerOnline && v.Status != store.ServerDraining && v.Status != store.ServerDisabled {
			return nil, huma.Error409Conflict("Only a draining or disabled server can go online. The server is " + v.Status + ".")
		}
	}
	if name != "" {
		if err := h.d.Store.SetServerName(ctx, v.ID, name); err != nil {
			return nil, h.storeErr(err, "No server has this ID.")
		}
	}
	if changeStatus {
		if err := h.d.Store.SetServerStatus(ctx, v.ID, *b.Status, ""); err != nil {
			return nil, h.storeErr(err, "No server has this ID.")
		}
	}
	old := v
	if v, err = h.d.Store.GetServer(ctx, v.ID); err != nil {
		return nil, h.storeErr(err, "No server has this ID.")
	}
	h.d.Bus.Publish(events.Event{Type: events.ServerState, Data: v})
	changes := map[string][2]any{}
	if v.Name != old.Name {
		changes["name"] = [2]any{old.Name, v.Name}
	}
	if v.Status != old.Status {
		changes["status"] = [2]any{old.Status, v.Status}
	}
	if len(changes) > 0 {
		h.audit(ctx, "server.update", v.ID, map[string]any{"name": v.Name, "status": v.Status, "changes": changes})
	}
	return h.serverOutput(ctx, v)
}

func (h *Handler) deleteServer(ctx context.Context, in *PathID) (*empty, error) {
	v, err := h.d.Store.GetServer(ctx, in.ID)
	if err != nil {
		return nil, h.storeErr(err, "No server has this ID.")
	}
	if v.Kind == store.ServerLocal {
		return nil, huma.Error409Conflict("The primary server cannot be removed.")
	}
	if h.d.Servers == nil {
		return nil, unavailable("Removing servers")
	}
	if err := h.d.Servers.Remove(ctx, v.ID); err != nil {
		return nil, h.serviceErr(err)
	}
	if err := h.d.Store.DeleteServer(ctx, v.ID); err != nil {
		return nil, h.storeErr(err, "No server has this ID.")
	}
	// The delete can change the servers of pools, or disable them.
	h.poolsChanged(ctx)
	h.audit(ctx, "server.delete", v.ID, map[string]any{"name": v.Name})
	return nil, nil
}

func (h *Handler) updateAgent(ctx context.Context, in *PathID) (*struct{ Body Server }, error) {
	if h.d.Servers == nil {
		return nil, unavailable("Agent updates")
	}
	v, err := h.d.Store.GetServer(ctx, in.ID)
	if err != nil {
		return nil, h.storeErr(err, "No server has this ID.")
	}
	switch h.d.Servers.UpdateState(v) {
	case UpdateCurrent:
		return nil, huma.Error409Conflict("The server runs runplane " + v.Version + ", the version of the primary.")
	case UpdateUpdating:
		return nil, huma.Error409Conflict("An update of the server runs.")
	case UpdateNone:
		return nil, huma.Error409Conflict("runplane cannot update the server: the primary is not a release build, or the server has not reported its version.")
	}
	if err := h.d.Servers.UpdateAgent(ctx, v.ID); err != nil {
		return nil, h.serviceErr(err)
	}
	h.audit(ctx, "server.update_agent", v.ID, map[string]any{"name": v.Name, "from": v.Version, "to": version.Version})
	return h.serverOutput(ctx, v)
}

type DoctorResult struct {
	ServerID  string     `json:"server_id"`
	CheckedAt *time.Time `json:"checked_at" doc:"Null when the doctor has not run on the server."`
	Checks    []Check    `json:"checks"`
}

func (h *Handler) getDoctor(ctx context.Context, in *PathID) (*struct{ Body DoctorResult }, error) {
	v, err := h.d.Store.GetServer(ctx, in.ID)
	if err != nil {
		return nil, h.storeErr(err, "No server has this ID.")
	}
	out := DoctorResult{ServerID: v.ID, Checks: []Check{}}
	r, err := h.d.Store.GetDoctorResult(ctx, v.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return &struct{ Body DoctorResult }{out}, nil
	case err != nil:
		return nil, h.internal(err)
	}
	out.CheckedAt = &r.CheckedAt
	for _, c := range r.Checks {
		out.Checks = append(out.Checks, Check(c))
	}
	return &struct{ Body DoctorResult }{out}, nil
}

func (h *Handler) runDoctor(ctx context.Context, in *PathID) (*struct{ Body DoctorResult }, error) {
	if h.d.Servers == nil {
		return nil, unavailable("The doctor")
	}
	v, err := h.d.Store.GetServer(ctx, in.ID)
	if err != nil {
		return nil, h.storeErr(err, "No server has this ID.")
	}
	checks, err := h.d.Servers.Doctor(ctx, v.ID)
	if err != nil {
		return nil, h.serviceErr(err)
	}
	out, err := h.saveDoctor(ctx, v.ID, checks)
	if err != nil {
		return nil, h.internal(err)
	}
	return &struct{ Body DoctorResult }{out}, nil
}

// saveDoctor stores checks as the last doctor result of the server.
func (h *Handler) saveDoctor(ctx context.Context, serverID string, checks []Check) (DoctorResult, error) {
	r := store.DoctorResult{ServerID: serverID, CheckedAt: time.Now().UTC().Truncate(time.Millisecond)}
	for _, c := range checks {
		r.Checks = append(r.Checks, store.DoctorCheck(c))
	}
	if err := h.d.Store.SaveDoctorResult(ctx, r); err != nil {
		return DoctorResult{}, err
	}
	return DoctorResult{ServerID: serverID, CheckedAt: &r.CheckedAt, Checks: orEmpty(checks)}, nil
}
