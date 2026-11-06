package api

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/danielgtaylor/huma/v2/sse"

	"github.com/runplane/runplane/internal/auth"
	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/store"
)

const pingEvery = 15 * time.Second

// stream is a bus event that the browser gets: the SSE event name, the API
// object of its data, and the conversion of the bus event into the data.
type stream struct {
	event   reflect.Type
	name    string
	data    any
	convert func(context.Context, events.Event) (any, error)
}

// errSkip is the error of a conversion when the browser does not get the
// event.
var errSkip = errors.New("the browser does not get this event")

func streamOf[E events.Event, D any](name string, convert func(context.Context, E) (D, error)) stream {
	var data D
	return stream{event: reflect.TypeFor[E](), name: name, data: data, convert: func(ctx context.Context, e events.Event) (any, error) {
		return convert(ctx, e.(E))
	}}
}

func same[E any](_ context.Context, e E) (E, error) { return e, nil }

func (h *Handler) streamList() []stream {
	return []stream{
		streamOf("sandbox.state", func(ctx context.Context, e events.SandboxState) (Sandbox, error) {
			return first(h.sandboxes(ctx, []store.Sandbox{store.Sandbox(e)}))
		}),
		streamOf("job.state", func(ctx context.Context, e events.JobState) (JobSummary, error) {
			return first(h.jobSummaries(ctx, []store.Job{store.Job(e)}))
		}),
		streamOf("pool.stats", same[events.PoolStats]),
		streamOf("pools.changed", same[events.PoolsChanged]),
		streamOf("server.state", func(ctx context.Context, e events.ServerState) (Server, error) {
			return first(h.servers(ctx, []store.Server{store.Server(e)}))
		}),
		streamOf("server.removed", same[events.ServerRemoved]),
		streamOf("host.stats", same[events.HostStats]),
		streamOf("runner.version", func(_ context.Context, e events.RunnerVersion) (RunnerVersion, error) {
			return runnerVersion(store.RunnerBundle(e), time.Now()), nil
		}),
		streamOf("image.state", func(ctx context.Context, e events.ImageState) (Image, error) {
			return h.imageEvent(ctx, e.ID)
		}),
		streamOf("image.state", func(ctx context.Context, e events.ImageBuildState) (Image, error) {
			return h.imageEvent(ctx, e.ImageID)
		}),
		streamOf("image.removed", same[events.ImageRemoved]),
		streamOf("github.state", func(ctx context.Context, _ events.GitHubState) (GitHubStatus, error) {
			return h.gitHubStatus(ctx)
		}),
		streamOf("settings.changed", func(ctx context.Context, e events.SettingsChanged) (Settings, error) {
			return h.settingsOut(ctx, e.New)
		}),
		// One stream route serves all roles.
		streamOf("backup.state", func(ctx context.Context, e events.BackupState) (Backup, error) {
			if !auth.CanDo(currentUser(ctx).Role, auth.ActionAdmin) {
				return Backup{}, errSkip
			}
			return h.backupOut(store.Backup(e)), nil
		}),
	}
}

// registerEvents registers the SSE route. sse.Sender finds the name of an
// event from the type of its data, so each name has its own data type.
func (h *Handler) registerEvents() {
	op := operation(auth.ActionRead, "GET /api/events", "stream-events", "Stream live events (SSE)", describe(
		"Server-Sent Events. Each event has the API object of its name as data. A comment line \": ping\" comes each 15 seconds. "+
			"Only admins get backup.state."))
	names := map[string]any{}
	byData := map[reflect.Type]string{}
	h.streams = map[reflect.Type]stream{}
	for _, s := range h.streamList() {
		data := reflect.TypeOf(s.data)
		if name, ok := byData[data]; ok && name != s.name {
			panic(fmt.Sprintf("the SSE events %s and %s have the same data type %s", name, s.name, data))
		}
		byData[data], names[s.name], h.streams[s.event] = s.name, s.data, s
	}
	sse.Register(h.api, op, names, h.streamEvents)
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
			s, ok := h.streams[reflect.TypeOf(e)]
			if !ok {
				continue
			}
			data, err := s.convert(ctx, e)
			if errors.Is(err, errSkip) {
				continue
			}
			if err != nil {
				h.d.Log.Warn("convert event", "type", fmt.Sprintf("%T", e), "err", err)
				continue
			}
			if send.Data(data) != nil {
				return
			}
		}
	}
}

// imageEvent reads the image again for its current builds. A removed image
// has no event.
func (h *Handler) imageEvent(ctx context.Context, id string) (Image, error) {
	img, err := h.d.Store.GetImage(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return Image{}, errSkip
	}
	if err != nil {
		return Image{}, err
	}
	return first(h.images(ctx, []store.Image{img}))
}
