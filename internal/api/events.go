package api

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/danielgtaylor/huma/v2/sse"

	"github.com/runplane/runplane/internal/auth"
	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/store"
)

type PoolStats struct {
	PoolID   string `json:"pool_id"`
	Idle     int    `json:"idle"`
	Busy     int    `json:"busy"`
	Starting int    `json:"starting"`
	Queued   int    `json:"queued"`
	Assigned int    `json:"assigned"`
}

type HostStats struct {
	ServerID      string    `json:"server_id"`
	At            time.Time `json:"at"`
	CPUPercent    float64   `json:"cpu_percent"`
	MemoryPercent float64   `json:"memory_percent"`
	DiskPercent   float64   `json:"disk_percent"`
}

const pingEvery = 15 * time.Second

func (h *Handler) registerEvents() {
	op := operation(auth.ActionRead, "GET /api/events", "stream-events", "Stream live events (SSE)", describe(
		"Server-Sent Events. Each event has the API object of its name as data. A comment line \": ping\" comes each 15 seconds. "+
			"Only admins get backup.state."))
	sse.Register(h.api, op, map[string]any{
		events.SandboxState:  Sandbox{},
		events.JobState:      JobSummary{},
		events.PoolStats:     PoolStats{},
		events.ServerState:   Server{},
		events.HostStats:     HostStats{},
		events.RunnerVersion: RunnerVersion{},
		events.ImageState:    Image{},
		events.GitHubState:   GitHubStatus{},
		events.BackupState:   Backup{},
	}, h.streamEvents)
}

func (h *Handler) streamEvents(ctx context.Context, _ *struct{}, send sse.Sender) {
	ch, cancel := h.d.Bus.Subscribe(256)
	defer cancel()
	ping := time.NewTicker(pingEvery)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.done:
			return
		case <-ping.C:
			if send.Comment("ping") != nil {
				return
			}
		case e, ok := <-ch:
			if !ok {
				return
			}
			data, err := h.eventData(ctx, e)
			if err != nil {
				h.d.Log.Warn("convert event", "type", e.Type, "err", err)
				continue
			}
			if data != nil && send.Data(data) != nil {
				return
			}
		}
	}
}

// eventData converts the data of a bus event into the API object of the
// event. Publishers can send the API object, the store model, or a value
// with the same JSON fields as the API object. It returns nil for events
// that the browser does not get.
func (h *Handler) eventData(ctx context.Context, e events.Event) (any, error) {
	switch e.Type {
	case events.SandboxState:
		if b, ok := events.As[store.Sandbox](e.Data); ok {
			return first(h.sandboxes(ctx, []store.Sandbox{b}))
		}
		return convert[Sandbox](e.Data)
	case events.JobState:
		if j, ok := events.As[store.Job](e.Data); ok {
			return first(h.jobSummaries(ctx, []store.Job{j}))
		}
		return convert[JobSummary](e.Data)
	case events.PoolStats:
		return convert[PoolStats](e.Data)
	case events.ServerState:
		if v, ok := events.As[store.Server](e.Data); ok {
			return first(h.servers(ctx, []store.Server{v}))
		}
		return convert[Server](e.Data)
	case events.HostStats:
		if st, ok := events.As[store.ServerStat](e.Data); ok {
			return HostStats(st), nil
		}
		return convert[HostStats](e.Data)
	case events.RunnerVersion:
		if b, ok := events.As[store.RunnerBundle](e.Data); ok {
			return runnerVersion(b, time.Now()), nil
		}
		return convert[RunnerVersion](e.Data)
	case events.ImageState:
		id := ""
		if img, ok := events.As[store.Image](e.Data); ok {
			id = img.ID
		} else if b, ok := events.As[store.ImageBuild](e.Data); ok {
			id = b.ImageID
		} else {
			return convert[Image](e.Data)
		}
		// Read the image again for its current builds.
		img, err := h.d.Store.GetImage(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return first(h.images(ctx, []store.Image{img}))
	case events.GitHubState:
		return h.gitHubStatus(ctx)
	case events.BackupState:
		if !auth.CanDo(currentUser(ctx).Role, auth.ActionAdmin) {
			return nil, nil
		}
		if b, ok := events.As[store.Backup](e.Data); ok {
			return h.backupOut(b), nil
		}
		return convert[Backup](e.Data)
	}
	return nil, nil
}

// convert returns v as a T, or through JSON for another type with the same fields.
func convert[T any](v any) (T, error) {
	if x, ok := events.As[T](v); ok {
		return x, nil
	}
	var out T
	b, err := json.Marshal(v)
	if err == nil {
		err = json.Unmarshal(b, &out)
	}
	return out, err
}
