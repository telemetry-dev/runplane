package store

import (
	"context"
	"fmt"
)

// FindLiveSandboxes returns the sandboxes that are not gone or failed and
// whose ID starts with prefix, newest first.
func (s *Store) FindLiveSandboxes(ctx context.Context, prefix string) ([]Sandbox, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT "+sandboxCols+` FROM sandboxes
		WHERE state IN ('starting', 'idle', 'busy', 'stopping') AND substr(id, 1, length(?1)) = ?1
		ORDER BY created_at DESC, id DESC`, prefix)
	list, err := scanAll(rows, err, scanSandbox)
	if err != nil {
		return nil, fmt.Errorf("find sandboxes: %w", err)
	}
	return list, nil
}
