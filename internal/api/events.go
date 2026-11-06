package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/danielgtaylor/huma/v2/sse"

	"github.com/runplane/runplane/internal/auth"
	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/store"
)

const pingEvery = 15 * time.Second

// registerEvents names the SSE events. sse.Sender finds the name of an event
// from the type of its data, so each name has its own data type.
func (h *Handler) registerEvents() {
	op := operation(auth.ActionRead, "GET /api/events", "stream-events", "Stream live events (SSE)", describe(
		"Server-Sent Events. Each event has the API object of its name as data. A comment line \": ping\" comes each 15 seconds. "+
			"Only admins get backup.state."))
	sse.Register(h.api, op, map[string]any{
		"sandbox.state":  Sandbox{},
		"job.state":      JobSummary{},
		"pool.stats":     events.PoolStats{},
		"pools.changed":  events.PoolsChanged{},
		"server.state":   Server{},
		"host.stats":     events.HostStats{},
		"runner.version": RunnerVersion{},
		"image.state":    Image{},
		"image.removed":  events.ImageRemoved{},
		"github.state":   GitHubStatus{},
		"backup.state":   Backup{},
	}, h.streamEvents)
}

// streamEvents ends when the browser reads the events too slowly. The
// browser then connects again and reads all its queries again.
func (h *Handler) streamEvents(ctx context.Context, _ *struct{}, send sse.Sender) {
	ch := h.d.Bus.Watch(ctx, 256)
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
				h.d.Log.Info("stop a slow event stream")
				return
			}
			data, err := h.eventData(ctx, e)
			if err != nil {
				h.d.Log.Warn("convert event", "type", fmt.Sprintf("%T", e), "err", err)
				continue
			}
			if data != nil && send.Data(data) != nil {
				return
			}
		}
	}
}

// eventData returns the API object of a bus event, or nil for an event that
// the browser does not get.
func (h *Handler) eventData(ctx context.Context, e events.Event) (any, error) {
	switch e := e.(type) {
	case events.SandboxState:
		return first(h.sandboxes(ctx, []store.Sandbox{store.Sandbox(e)}))
	case events.JobState:
		return first(h.jobSummaries(ctx, []store.Job{store.Job(e)}))
	case events.PoolStats, events.PoolsChanged, events.HostStats, events.ImageRemoved:
		return e, nil
	case events.ServerState:
		return first(h.servers(ctx, []store.Server{store.Server(e)}))
	case events.RunnerVersion:
		return runnerVersion(store.RunnerBundle(e), time.Now()), nil
	case events.ImageState:
		return h.imageEvent(ctx, e.ID)
	case events.ImageBuildState:
		return h.imageEvent(ctx, e.ImageID)
	case events.GitHubState:
		return h.gitHubStatus(ctx)
	case events.BackupState:
		if !auth.CanDo(currentUser(ctx).Role, auth.ActionAdmin) {
			return nil, nil
		}
		return h.backupOut(store.Backup(e)), nil
	}
	return nil, nil
}

// imageEvent reads the image again for its current builds.
func (h *Handler) imageEvent(ctx context.Context, id string) (any, error) {
	img, err := h.d.Store.GetImage(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return first(h.images(ctx, []store.Image{img}))
}
