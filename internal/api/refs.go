package api

import (
	"context"

	"github.com/runplane/runplane/internal/store"
)

// refs holds the pools and servers by ID, for the labels and names that
// API objects show next to the IDs. Both tables are small.
type refs struct {
	pools   map[string]store.Pool
	servers map[string]store.Server
}

func (h *Handler) loadRefs(ctx context.Context) (refs, error) {
	pools, err := h.d.Store.ListPools(ctx)
	if err != nil {
		return refs{}, err
	}
	servers, err := h.d.Store.ListServers(ctx)
	if err != nil {
		return refs{}, err
	}
	r := refs{pools: make(map[string]store.Pool, len(pools)), servers: make(map[string]store.Server, len(servers))}
	for _, p := range pools {
		r.pools[p.ID] = p
	}
	for _, v := range servers {
		r.servers[v.ID] = v
	}
	return r, nil
}

func (r refs) poolLabel(id string) string  { return r.pools[id].Label }
func (r refs) serverName(id string) string { return r.servers[id].Name }
