// Package pools changes the pools: the runner labels with their sandbox size
// and image. Each change records an audit entry and publishes
// events.PoolsChanged, which the scheduler and the GitHub scale sets follow.
package pools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/runplane/runplane/internal/audit"
	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/field"
	"github.com/runplane/runplane/internal/ids"
	"github.com/runplane/runplane/internal/store"
)

type Config struct {
	Store *store.Store
	Bus   *events.Bus
	Arch  string       // of the primary server, the default arch of a pool
	Log   *slog.Logger // nil: no log
}

type Service struct {
	st    *store.Store
	bus   *events.Bus
	arch  string
	audit audit.Log
	log   *slog.Logger
}

func New(c Config) *Service {
	return &Service{
		st: c.Store, bus: c.Bus, arch: c.Arch, audit: audit.Log{Store: c.Store, Logger: c.Log},
		log: cmp.Or(c.Log, slog.New(slog.DiscardHandler)),
	}
}

// Create adds the pool p. Its name defaults to the label, and its arch to the
// arch of the primary server. A refused value gives field.Problems, and a
// label in use store.ErrConflict.
func (s *Service) Create(ctx context.Context, by audit.Actor, p store.Pool) (store.Pool, error) {
	now := time.Now()
	p.ID, p.CreatedAt, p.UpdatedAt = ids.New(), now, now
	p.Name = cmp.Or(strings.TrimSpace(p.Name), p.Label)
	p.Arch = cmp.Or(p.Arch, s.arch)
	p.ServerIDs = orEmpty(p.ServerIDs)
	if err := s.check(ctx, p, nil); err != nil {
		return store.Pool{}, err
	}
	if err := s.st.CreatePool(ctx, p); err != nil {
		return store.Pool{}, err
	}
	s.bus.Publish(events.PoolsChanged{})
	s.audit.Record(ctx, by, audit.PoolCreate, p.ID, map[string]any{"label": p.Label})
	return p, nil
}

func (s *Service) Update(ctx context.Context, by audit.Actor, id string, change func(*store.Pool)) (store.Pool, error) {
	old, err := s.st.GetPool(ctx, id)
	if err != nil {
		return store.Pool{}, err
	}
	old.ServerIDs = orEmpty(old.ServerIDs)
	p := old
	p.ServerIDs = slices.Clone(old.ServerIDs)
	change(&p)
	p.Name, p.ServerIDs = strings.TrimSpace(p.Name), orEmpty(p.ServerIDs)
	if err := s.check(ctx, p, old.ServerIDs); err != nil {
		return store.Pool{}, err
	}
	p.UpdatedAt = time.Now()
	if err := s.st.UpdatePool(ctx, p); err != nil {
		return store.Pool{}, err
	}
	s.bus.Publish(events.PoolsChanged{})
	if changes := s.changes(ctx, old, p); len(changes) > 0 {
		s.audit.Record(ctx, by, audit.PoolUpdate, p.ID, map[string]any{"label": p.Label, "changes": changes})
	}
	return p, nil
}

// Delete deletes the pool id. The scheduler stops the sandboxes of the pool
// and the GitHub code deletes its scale sets when they see that the pool is
// gone.
func (s *Service) Delete(ctx context.Context, by audit.Actor, id string) error {
	p, err := s.st.GetPool(ctx, id)
	if err != nil {
		return err
	}
	if err := s.st.DeletePool(ctx, p.ID); err != nil {
		return err
	}
	s.bus.Publish(events.PoolsChanged{})
	s.audit.Record(ctx, by, audit.PoolDelete, p.ID, map[string]any{"label": p.Label})
	return nil
}

// check checks the rules that need more than one field or the database.
// saved holds the server IDs of the stored pool: an unknown ID among them is
// from a server that no longer exists, and it does not block the change.
func (s *Service) check(ctx context.Context, p store.Pool, saved []string) error {
	var errs field.Problems
	if p.Name == "" {
		errs.Add("name", "Enter a name.", p.Name)
	}
	if p.MinIdle > p.MaxRunners {
		errs.Add("min_idle", "Warm runners cannot be more than max runners.", p.MinIdle)
	}
	if p.ImageID != "" {
		if _, err := s.st.GetImage(ctx, p.ImageID); errors.Is(err, store.ErrNotFound) {
			errs.Add("image_id", "No image has this ID.", p.ImageID)
		} else if err != nil {
			return err
		}
	}
	for i, id := range p.ServerIDs {
		if slices.Contains(saved, id) {
			continue
		}
		if _, err := s.st.GetServer(ctx, id); errors.Is(err, store.ErrNotFound) {
			errs.Add(fmt.Sprintf("server_ids[%d]", i), "No server has this ID.", id)
		} else if err != nil {
			return err
		}
	}
	return errs.Err()
}

// changes gives the old and new value of each changed field of a pool, with
// the names of the image and the servers in place of their IDs. An image or
// server that runplane cannot name keeps its ID. An empty image is the
// default image.
func (s *Service) changes(ctx context.Context, old, p store.Pool) map[string][2]any {
	changes := audit.Changes(old, p, "updated_at")
	if _, ok := changes["image_id"]; ok {
		names := map[string]string{}
		images, err := s.st.ListImages(ctx)
		if err != nil {
			s.log.Warn("read the image names for the audit log", "err", err)
		}
		for _, img := range images {
			names[img.ID] = img.Name
		}
		delete(changes, "image_id")
		changes["image"] = [2]any{cmp.Or(names[old.ImageID], old.ImageID), cmp.Or(names[p.ImageID], p.ImageID)}
	}
	if _, ok := changes["server_ids"]; ok {
		names := map[string]string{}
		servers, err := s.st.ListServers(ctx)
		if err != nil {
			s.log.Warn("read the server names for the audit log", "err", err)
		}
		for _, v := range servers {
			names[v.ID] = v.Name
		}
		named := func(ids []string) []string {
			out := make([]string, len(ids))
			for i, id := range ids {
				out[i] = cmp.Or(names[id], id)
			}
			return out
		}
		delete(changes, "server_ids")
		changes["servers"] = [2]any{named(old.ServerIDs), named(p.ServerIDs)}
	}
	return changes
}

// orEmpty gives an empty list for nil, because the JSON of the two differs
// and an empty server_ids means all servers.
func orEmpty(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}
