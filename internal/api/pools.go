package api

import (
	"cmp"
	"context"
	"errors"
	"time"

	"github.com/danielgtaylor/huma/v2"

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
	Assigned int `json:"assigned" doc:"Of queued, the jobs that GitHub assigned to the scale set of the pool."`

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
			Idle: c.Idle, Busy: c.Busy, Starting: c.Starting, Queued: c.Queued, Assigned: c.Assigned,
			PoolPlacement: placer.place(p),
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

// PoolSize is the size of the sandboxes of a pool. The pool requests and the
// placement query have it, with the same limits.
type PoolSize struct {
	VCPU     int `json:"vcpu" query:"vcpu" required:"true" minimum:"1" maximum:"64"`
	MemoryMB int `json:"memory_mb" query:"memory_mb" required:"true" minimum:"512" maximum:"262144"`
}

type CreatePoolRequest struct {
	Name  string `json:"name,omitempty" maxLength:"64" doc:"Defaults to the label."`
	Label string `json:"label" maxLength:"63" pattern:"^[a-z0-9][a-z0-9-]{0,62}$" patternDescription:"lowercase letters, digits, and dashes" doc:"The runs-on label. Unique."`
	Arch  string `json:"arch,omitempty" enum:"x64,arm64" doc:"Defaults to the arch of the primary server."`
	PoolSize
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
	p, err := h.d.Pools.Create(ctx, actor(ctx), store.Pool{
		Name: b.Name, Label: b.Label, Arch: b.Arch, VCPU: b.VCPU, MemoryMB: b.MemoryMB, DiskGB: b.DiskGB,
		ImageID: b.ImageID, MinIdle: b.MinIdle, MaxRunners: b.MaxRunners, TimeLimitMinutes: b.TimeLimitMinutes,
		ServerIDs: b.ServerIDs, Enabled: b.Enabled == nil || *b.Enabled,
	})
	if err != nil {
		return nil, poolErr(err)
	}
	return reply(first(h.pools(ctx, []store.Pool{p})))
}

// UpdatePoolRequest has the limits of CreatePoolRequest.
type UpdatePoolRequest struct {
	Name             *string   `json:"name,omitempty"`
	Label            *string   `json:"label,omitempty"`
	Arch             *string   `json:"arch,omitempty"`
	VCPU             *int      `json:"vcpu,omitempty"`
	MemoryMB         *int      `json:"memory_mb,omitempty"`
	DiskGB           *int      `json:"disk_gb,omitempty"`
	ImageID          *string   `json:"image_id,omitempty" doc:"Empty: the default image."`
	MinIdle          *int      `json:"min_idle,omitempty"`
	MaxRunners       *int      `json:"max_runners,omitempty"`
	TimeLimitMinutes *int      `json:"time_limit_minutes,omitempty"`
	ServerIDs        *[]string `json:"server_ids,omitempty"`
	Enabled          *bool     `json:"enabled,omitempty"`
}

func (UpdatePoolRequest) TransformSchema(r huma.Registry, s *huma.Schema) *huma.Schema {
	return patchOf[CreatePoolRequest](r, s)
}

func (h *Handler) updatePool(ctx context.Context, in *struct {
	PathID
	Body UpdatePoolRequest
}) (*body[Pool], error) {
	b := in.Body
	p, err := h.d.Pools.Update(ctx, actor(ctx), in.ID, func(p *store.Pool) {
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
	})
	if err != nil {
		return nil, poolErr(err)
	}
	return reply(first(h.pools(ctx, []store.Pool{p})))
}

func (h *Handler) deletePool(ctx context.Context, in *PathID) (*empty, error) {
	if err := h.d.Pools.Delete(ctx, actor(ctx), in.ID); err != nil {
		return nil, notFound(err, "No pool has this ID.")
	}
	return nil, nil
}

type placementInput struct {
	Arch string `query:"arch" required:"true" enum:"x64,arm64"`
	PoolSize
	ServerIDs []string `query:"server_ids" maxItems:"100" doc:"Comma-separated. Empty: all servers."`
}

// getPlacement answers the placement of a size that is not saved, for
// the hint of the pool editor.
func (h *Handler) getPlacement(ctx context.Context, in *placementInput) (*body[PoolPlacement], error) {
	placer, err := h.newPlacer(ctx)
	if err != nil {
		return nil, err
	}
	pool := store.Pool{Arch: in.Arch, VCPU: in.VCPU, MemoryMB: in.MemoryMB, ServerIDs: in.ServerIDs}
	return &body[PoolPlacement]{placer.place(pool)}, nil
}

type placer struct {
	servers []store.Server
	reserve sandbox.Reserve
	// limits are the limits of the drivers of the connected servers.
	limits map[string]sandbox.Limits
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
	p := placer{reserve: settings.Reserve(), limits: map[string]sandbox.Limits{}}
	for _, v := range list {
		if v.Status != store.ServerOnline {
			continue
		}
		p.servers = append(p.servers, v)
		if l, ok := h.d.Servers.Limits(v.ID); ok {
			p.limits[v.ID] = l
		}
	}
	return p, nil
}

func (p placer) place(pool store.Pool) PoolPlacement {
	out := PoolPlacement{PlaceableServerIDs: []string{}}
	for _, v := range p.servers {
		if !pool.CanUse(v.ID, v.Arch) {
			continue
		}
		out.OnlineServers++
		empty := sandbox.Capacity{VCPU: v.VCPU, MemoryMB: v.MemoryMB, Limits: p.limits[v.ID]}
		if empty.Room(pool.VCPU, pool.MemoryMB, p.reserve) > 0 {
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
