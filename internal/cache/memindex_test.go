package cache

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"sync"
	"time"
)

// MemIndex is an Index in memory. It loses all entries when the process stops.
type MemIndex struct {
	mu      sync.Mutex
	entries map[string]Entry
}

func NewMemIndex() *MemIndex {
	return &MemIndex{entries: map[string]Entry{}}
}

func (m *MemIndex) Get(_ context.Context, id string) (Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[id]
	if !ok {
		return Entry{}, ErrNotFound
	}
	return e, nil
}

func (m *MemIndex) Find(_ context.Context, repository, ref, key, version string) (Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.entries {
		if e.Repository == repository && e.Ref == ref && e.Key == key && e.Version == version {
			return e, nil
		}
	}
	return Entry{}, ErrNotFound
}

func (m *MemIndex) Lookup(_ context.Context, q Query) (Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best Entry
	found := false
	for _, e := range m.entries {
		if !e.Finalized || e.Repository != q.Repository || e.Ref != q.Ref || e.Version != q.Version {
			continue
		}
		if e.Key != q.Key && !(q.Prefix && strings.HasPrefix(e.Key, q.Key)) {
			continue
		}
		if !found || newest(e, best) < 0 {
			best, found = e, true
		}
	}
	if !found {
		return Entry{}, ErrNotFound
	}
	return best, nil
}

func (m *MemIndex) Create(_ context.Context, e Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, old := range m.entries {
		if old.ID == e.ID || old.Repository == e.Repository && old.Ref == e.Ref && old.Key == e.Key && old.Version == e.Version {
			return ErrExists
		}
	}
	m.entries[e.ID] = e
	return nil
}

func (m *MemIndex) Finalize(_ context.Context, id string, size int64, at time.Time) error {
	return m.update(id, func(e *Entry) {
		e.Finalized = true
		e.SizeBytes = size
		e.LastUsedAt = at
	})
}

func (m *MemIndex) Touch(_ context.Context, id string, at time.Time) error {
	return m.update(id, func(e *Entry) { e.LastUsedAt = at })
}

func (m *MemIndex) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.entries[id]; !ok {
		return ErrNotFound
	}
	delete(m.entries, id)
	return nil
}

func (m *MemIndex) List(_ context.Context, repository string) ([]Entry, error) {
	list := m.filter(func(e Entry) bool { return e.Repository == repository })
	slices.SortFunc(list, newest)
	return list, nil
}

func (m *MemIndex) Usage(_ context.Context) ([]Usage, error) {
	m.mu.Lock()
	byRepo := map[string]*Usage{}
	for _, e := range m.entries {
		if !e.Finalized {
			continue
		}
		u := byRepo[e.Repository]
		if u == nil {
			u = &Usage{Repository: e.Repository}
			byRepo[e.Repository] = u
		}
		u.Entries++
		u.Bytes += e.SizeBytes
	}
	m.mu.Unlock()
	list := make([]Usage, 0, len(byRepo))
	for _, u := range byRepo {
		list = append(list, *u)
	}
	slices.SortFunc(list, func(a, b Usage) int { return strings.Compare(a.Repository, b.Repository) })
	return list, nil
}

func (m *MemIndex) Oldest(_ context.Context, limit int) ([]Entry, error) {
	list := m.filter(func(e Entry) bool { return e.Finalized })
	slices.SortFunc(list, func(a, b Entry) int {
		return cmp.Or(a.LastUsedAt.Compare(b.LastUsedAt), strings.Compare(a.ID, b.ID))
	})
	return list[:min(limit, len(list))], nil
}

func (m *MemIndex) Unfinished(_ context.Context, before time.Time) ([]Entry, error) {
	return m.filter(func(e Entry) bool { return !e.Finalized && e.CreatedAt.Before(before) }), nil
}

func (m *MemIndex) update(id string, change func(*Entry)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[id]
	if !ok {
		return ErrNotFound
	}
	change(&e)
	m.entries[id] = e
	return nil
}

func (m *MemIndex) filter(keep func(Entry) bool) []Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	var list []Entry
	for _, e := range m.entries {
		if keep(e) {
			list = append(list, e)
		}
	}
	return list
}

// newest orders entries newest first. ULIDs break ties because they sort by time.
func newest(a, b Entry) int {
	return cmp.Or(b.CreatedAt.Compare(a.CreatedAt), strings.Compare(b.ID, a.ID))
}
