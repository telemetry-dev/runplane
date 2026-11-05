package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/runplane/runplane/internal/ids"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

type Pool struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Label            string    `json:"label" doc:"The runs-on label of the pool."`
	Arch             string    `json:"arch" enum:"x64,arm64"`
	VCPU             int       `json:"vcpu"`
	MemoryMB         int       `json:"memory_mb"`
	DiskGB           int       `json:"disk_gb" doc:"Size of the scratch disk."`
	ImageID          string    `json:"image_id" doc:"Empty: the default image."`
	ImageName        string    `json:"image_name" doc:"Name of the image of the pool, or of the default image."`
	MinIdle          int       `json:"min_idle" doc:"Warm sandboxes that wait for jobs."`
	MaxRunners       int       `json:"max_runners"`
	TimeLimitMinutes int       `json:"time_limit_minutes" doc:"0: the default_time_limit_minutes setting."`
	ServerIDs        []string  `json:"server_ids" doc:"Servers that may run the sandboxes. Empty: all servers with the arch of the pool."`
	Enabled          bool      `json:"enabled"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`

	Idle     int `json:"idle" doc:"Live count of idle sandboxes."`
	Busy     int `json:"busy" doc:"Live count of sandboxes that run a job."`
	Starting int `json:"starting" doc:"Live count of sandboxes that start."`
	Queued   int `json:"queued" doc:"Jobs of the last 24 hours that wait for a runner."`

	PoolPlacement
}

// PoolPlacement tells on which servers a sandbox of a pool size can start.
// It compares the size with the totals of each server, not with its free
// resources now.
type PoolPlacement struct {
	OnlineServers      int      `json:"online_servers" doc:"Online servers with the arch, limited to server_ids if the pool has them."`
	PlaceableServers   int      `json:"placeable_servers" doc:"Of online_servers, the servers whose vCPUs and memory, after the reserve, the CPU overcommit, and the limits of the driver, fit one sandbox. 0 means that no sandbox can start."`
	PlaceableServerIDs []string `json:"placeable_server_ids"`
}

// pools converts store pools and adds the image names, live counts, and placement.
func (h *Handler) pools(ctx context.Context, list []store.Pool) ([]Pool, error) {
	images, err := h.d.Store.ListImages(ctx)
	if err != nil {
		return nil, err
	}
	placer, err := h.newPlacer(ctx)
	if err != nil {
		return nil, err
	}
	imageNames := map[string]string{}
	var defaultImage string
	for _, img := range images {
		imageNames[img.ID] = img.Name
		if img.IsDefault {
			defaultImage = img.Name
		}
	}
	counts, err := h.d.Store.PoolCounts(ctx, time.Now())
	if err != nil {
		return nil, err
	}
	out := make([]Pool, len(list))
	for i, p := range list {
		c := counts[p.ID]
		out[i] = Pool{
			ID: p.ID, Name: p.Name, Label: p.Label, Arch: p.Arch, VCPU: p.VCPU, MemoryMB: p.MemoryMB, DiskGB: p.DiskGB,
			ImageID: p.ImageID, ImageName: cmp.Or(imageNames[p.ImageID], defaultImage), MinIdle: p.MinIdle,
			MaxRunners: p.MaxRunners, TimeLimitMinutes: p.TimeLimitMinutes, ServerIDs: orEmpty(p.ServerIDs),
			Enabled: p.Enabled, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
			Idle: c.Idle, Busy: c.Busy, Starting: c.Starting, Queued: c.Queued,
			PoolPlacement: placer.place(p.Arch, p.VCPU, p.MemoryMB, p.ServerIDs),
		}
	}
	return out, nil
}

func (h *Handler) listPools(ctx context.Context, _ *struct{}) (*body[[]Pool], error) {
	list, err := h.d.Store.ListPools(ctx)
	if err != nil {
		return nil, err
	}
	return reply(h.pools(ctx, list))
}

func (h *Handler) getPool(ctx context.Context, in *PathID) (*body[Pool], error) {
	p, err := h.d.Store.GetPool(ctx, in.ID)
	if err != nil {
		return nil, notFound(err, "No pool has this ID.")
	}
	return reply(first(h.pools(ctx, []store.Pool{p})))
}

type CreatePoolRequest struct {
	Name             string   `json:"name,omitempty" maxLength:"64" doc:"Defaults to the label."`
	Label            string   `json:"label" maxLength:"63" pattern:"^[a-z0-9][a-z0-9-]{0,62}$" patternDescription:"lowercase letters, digits, and dashes" doc:"The runs-on label. Unique."`
	Arch             string   `json:"arch,omitempty" enum:"x64,arm64" doc:"Defaults to the arch of the primary server."`
	VCPU             int      `json:"vcpu" minimum:"1" maximum:"64"`
	MemoryMB         int      `json:"memory_mb" minimum:"512" maximum:"262144"`
	DiskGB           int      `json:"disk_gb" minimum:"8" maximum:"1024"`
	ImageID          string   `json:"image_id,omitempty" maxLength:"100" doc:"Empty: the default image."`
	MinIdle          int      `json:"min_idle,omitempty" minimum:"0" maximum:"1000"`
	MaxRunners       int      `json:"max_runners" minimum:"1" maximum:"1000"`
	TimeLimitMinutes int      `json:"time_limit_minutes,omitempty" minimum:"0" maximum:"7200" doc:"0: the default_time_limit_minutes setting."`
	ServerIDs        []string `json:"server_ids,omitempty" maxItems:"100"`
	Enabled          *bool    `json:"enabled,omitempty" doc:"Defaults to true."`
}

func (h *Handler) createPool(ctx context.Context, in *body[CreatePoolRequest]) (*body[Pool], error) {
	b := in.Body
	now := time.Now()
	p := store.Pool{
		ID: ids.New(), Name: cmp.Or(strings.TrimSpace(b.Name), b.Label), Label: b.Label, Arch: b.Arch,
		VCPU: b.VCPU, MemoryMB: b.MemoryMB, DiskGB: b.DiskGB, ImageID: b.ImageID, MinIdle: b.MinIdle,
		MaxRunners: b.MaxRunners, TimeLimitMinutes: b.TimeLimitMinutes, ServerIDs: orEmpty(b.ServerIDs),
		Enabled: b.Enabled == nil || *b.Enabled, CreatedAt: now, UpdatedAt: now,
	}
	if p.Arch == "" {
		arch, err := h.defaultArch(ctx)
		if err != nil {
			return nil, err
		}
		p.Arch = arch
	}
	if err := h.checkPool(ctx, p, nil); err != nil {
		return nil, err
	}
	if err := h.d.Store.CreatePool(ctx, p); err != nil {
		return nil, poolErr(err)
	}
	h.poolsChanged()
	h.audit(ctx, "pool.create", p.ID, map[string]any{"label": p.Label})
	return reply(first(h.pools(ctx, []store.Pool{p})))
}

type UpdatePoolRequest struct {
	Name             *string   `json:"name,omitempty" minLength:"1" maxLength:"64"`
	Label            *string   `json:"label,omitempty" maxLength:"63" pattern:"^[a-z0-9][a-z0-9-]{0,62}$" patternDescription:"lowercase letters, digits, and dashes"`
	Arch             *string   `json:"arch,omitempty" enum:"x64,arm64"`
	VCPU             *int      `json:"vcpu,omitempty" minimum:"1" maximum:"64"`
	MemoryMB         *int      `json:"memory_mb,omitempty" minimum:"512" maximum:"262144"`
	DiskGB           *int      `json:"disk_gb,omitempty" minimum:"8" maximum:"1024"`
	ImageID          *string   `json:"image_id,omitempty" maxLength:"100" doc:"Empty: the default image."`
	MinIdle          *int      `json:"min_idle,omitempty" minimum:"0" maximum:"1000"`
	MaxRunners       *int      `json:"max_runners,omitempty" minimum:"1" maximum:"1000"`
	TimeLimitMinutes *int      `json:"time_limit_minutes,omitempty" minimum:"0" maximum:"7200"`
	ServerIDs        *[]string `json:"server_ids,omitempty" maxItems:"100"`
	Enabled          *bool     `json:"enabled,omitempty"`
}

func (h *Handler) updatePool(ctx context.Context, in *struct {
	PathID
	Body UpdatePoolRequest
}) (*body[Pool], error) {
	p, err := h.d.Store.GetPool(ctx, in.ID)
	if err != nil {
		return nil, notFound(err, "No pool has this ID.")
	}
	old := p
	b := in.Body
	set(&p.Name, b.Name)
	set(&p.Label, b.Label)
	set(&p.Arch, b.Arch)
	set(&p.VCPU, b.VCPU)
	set(&p.MemoryMB, b.MemoryMB)
	set(&p.DiskGB, b.DiskGB)
	set(&p.ImageID, b.ImageID)
	set(&p.MinIdle, b.MinIdle)
	set(&p.MaxRunners, b.MaxRunners)
	set(&p.TimeLimitMinutes, b.TimeLimitMinutes)
	set(&p.ServerIDs, b.ServerIDs)
	set(&p.Enabled, b.Enabled)
	p.Name = strings.TrimSpace(p.Name)
	if err := h.checkPool(ctx, p, old.ServerIDs); err != nil {
		return nil, err
	}
	p.UpdatedAt = time.Now()
	if err := h.d.Store.UpdatePool(ctx, p); err != nil {
		return nil, poolErr(err)
	}
	h.poolsChanged()
	if changes := h.poolChanges(ctx, old, p); len(changes) > 0 {
		h.audit(ctx, "pool.update", p.ID, map[string]any{"label": p.Label, "changes": changes})
	}
	return reply(first(h.pools(ctx, []store.Pool{p})))
}

// poolChanges gives the old and new value of each changed field of a pool,
// with the names of the image and the servers in place of their IDs. An
// image or server that runplane cannot name keeps its ID. An empty image is
// the default image.
func (h *Handler) poolChanges(ctx context.Context, old, p store.Pool) map[string][2]any {
	old.ServerIDs, p.ServerIDs = orEmpty(old.ServerIDs), orEmpty(p.ServerIDs)
	changes := fieldChanges(old, p, "updated_at")
	if _, ok := changes["image_id"]; ok {
		names := map[string]string{}
		images, err := h.d.Store.ListImages(ctx)
		if err != nil {
			h.d.Log.Warn("read the image names for the audit log", "err", err)
		}
		for _, img := range images {
			names[img.ID] = img.Name
		}
		delete(changes, "image_id")
		changes["image"] = [2]any{cmp.Or(names[old.ImageID], old.ImageID), cmp.Or(names[p.ImageID], p.ImageID)}
	}
	if _, ok := changes["server_ids"]; ok {
		r, err := h.loadRefs(ctx)
		if err != nil {
			h.d.Log.Warn("read the server names for the audit log", "err", err)
		}
		names := func(ids []string) []string {
			out := make([]string, len(ids))
			for i, id := range ids {
				out[i] = cmp.Or(r.servers[id].Name, id)
			}
			return out
		}
		delete(changes, "server_ids")
		changes["servers"] = [2]any{names(old.ServerIDs), names(p.ServerIDs)}
	}
	return changes
}

func (h *Handler) deletePool(ctx context.Context, in *PathID) (*empty, error) {
	p, err := h.d.Store.GetPool(ctx, in.ID)
	if err != nil {
		return nil, notFound(err, "No pool has this ID.")
	}
	// The scheduler stops the sandboxes of the pool and the GitHub code
	// deletes its scale sets when they see that the pool is gone.
	if err := h.d.Store.DeletePool(ctx, p.ID); err != nil {
		return nil, notFound(err, "No pool has this ID.")
	}
	h.poolsChanged()
	h.audit(ctx, "pool.delete", p.ID, map[string]any{"label": p.Label})
	return nil, nil
}

// checkPool checks the rules that need more than one field or the database.
// saved holds the server IDs of the stored pool: an unknown ID among them is
// from a server that no longer exists, and it does not block the change.
func (h *Handler) checkPool(ctx context.Context, p store.Pool, saved []string) error {
	var errs problems
	if p.Name == "" {
		errs.add("body.name", "Enter a name.", p.Name)
	}
	if p.MinIdle > p.MaxRunners {
		errs.add("body.min_idle", "Warm runners cannot be more than max runners.", p.MinIdle)
	}
	if p.ImageID != "" {
		if _, err := h.d.Store.GetImage(ctx, p.ImageID); errors.Is(err, store.ErrNotFound) {
			errs.add("body.image_id", "No image has this ID.", p.ImageID)
		} else if err != nil {
			return err
		}
	}
	for i, id := range p.ServerIDs {
		if slices.Contains(saved, id) {
			continue
		}
		if _, err := h.d.Store.GetServer(ctx, id); errors.Is(err, store.ErrNotFound) {
			errs.add(fmt.Sprintf("body.server_ids[%d]", i), "No server has this ID.", id)
		} else if err != nil {
			return err
		}
	}
	return errs.err()
}

type placementInput struct {
	Arch      string   `query:"arch" required:"true" enum:"x64,arm64"`
	VCPU      int      `query:"vcpu" required:"true" minimum:"1" maximum:"64"`
	MemoryMB  int      `query:"memory_mb" required:"true" minimum:"512" maximum:"262144"`
	ServerIDs []string `query:"server_ids" maxItems:"100" doc:"Comma-separated. Empty: all servers."`
}

// getPlacement answers the placement of a size that is not saved, for
// the hint of the pool editor.
func (h *Handler) getPlacement(ctx context.Context, in *placementInput) (*body[PoolPlacement], error) {
	placer, err := h.newPlacer(ctx)
	if err != nil {
		return nil, err
	}
	return &body[PoolPlacement]{placer.place(in.Arch, in.VCPU, in.MemoryMB, in.ServerIDs)}, nil
}

type placer struct {
	servers []store.Server
	reserve sandbox.Reserve
	// limits are the capacities of the connected servers, for the limits
	// of their drivers.
	limits map[string]sandbox.Capacity
}

func (h *Handler) newPlacer(ctx context.Context) (placer, error) {
	settings, err := h.d.Store.GetSettings(ctx)
	if err != nil {
		return placer{}, err
	}
	list, err := h.d.Store.ListServers(ctx)
	if err != nil {
		return placer{}, err
	}
	p := placer{reserve: settings.Reserve(), limits: map[string]sandbox.Capacity{}}
	for _, v := range list {
		if v.Status != store.ServerOnline {
			continue
		}
		p.servers = append(p.servers, v)
		if h.d.Servers == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		if c, err := h.d.Servers.Capacity(ctx, v.ID); err == nil {
			p.limits[v.ID] = c
		}
		cancel()
	}
	return p, nil
}

func (p placer) place(arch string, vcpu, memoryMB int, serverIDs []string) PoolPlacement {
	out := PoolPlacement{PlaceableServerIDs: []string{}}
	for _, v := range p.servers {
		if v.Arch != arch || len(serverIDs) > 0 && !slices.Contains(serverIDs, v.ID) {
			continue
		}
		out.OnlineServers++
		l := p.limits[v.ID]
		empty := sandbox.Capacity{VCPU: v.VCPU, MemoryMB: v.MemoryMB, OverheadMB: l.OverheadMB, MaxVCPU: l.MaxVCPU, MaxSandboxes: l.MaxSandboxes}
		if empty.Room(vcpu, memoryMB, p.reserve) > 0 {
			out.PlaceableServerIDs = append(out.PlaceableServerIDs, v.ID)
		}
	}
	out.PlaceableServers = len(out.PlaceableServerIDs)
	return out
}

func poolErr(err error) error {
	if errors.Is(err, store.ErrConflict) {
		return conflict("body.label", "A pool with this label exists.")
	}
	return notFound(err, "No pool has this ID.")
}

func (h *Handler) poolsChanged() {
	if h.d.Scheduler != nil {
		h.d.Scheduler.PoolsChanged()
	}
}

// defaultArch is the arch of the primary server, or of this binary before
// the primary has reported its facts.
func (h *Handler) defaultArch(ctx context.Context) (string, error) {
	local, err := h.d.Store.GetLocalServer(ctx)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	if local.Arch != "" {
		return local.Arch, nil
	}
	if runtime.GOARCH == "arm64" {
		return "arm64", nil
	}
	return "x64", nil
}
