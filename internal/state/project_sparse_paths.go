package state

import (
	"context"
	"fmt"
)

// SparsePathsForProject reads a project's configured cone-mode
// sparse-checkout directories, ordered for stable output. An empty result
// means no profile is configured — materialization leaves the working tree
// full. This table is deliberately local-only (see migration
// 00033_project_sparse_paths.sql): it is never event-sourced and never
// synced, so each device's sparse profile is its own. Routed through the
// reader pool: a pure SELECT with no write/transaction involved.
func (s *Store) SparsePathsForProject(ctx context.Context, namespaceID string) ([]string, error) {
	rows, err := s.reader().QueryContext(ctx, `
SELECT path FROM project_sparse_paths WHERE namespace_id = ? ORDER BY path;
`, namespaceID)
	if err != nil {
		return nil, fmt.Errorf("read project sparse paths: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan project sparse path: %w", err)
		}
		paths = append(paths, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate project sparse paths: %w", err)
	}
	return paths, nil
}

// ReplaceSparsePathsForProject sets a project's configured cone-mode
// sparse-checkout directories to exactly paths, replacing any previous set in
// one transaction. An empty/nil paths clears the profile entirely (equivalent
// to "no sparse-checkout configured" — the caller is responsible for also
// running SparseCheckoutDisable against any already-materialized checkout;
// this method only persists the DB-side desired state). Paths are assumed
// already cleaned/validated by the caller (internal/git's CleanSparsePath /
// ValidSparsePath) — this is a storage primitive, not a validation boundary.
func (s *Store) ReplaceSparsePathsForProject(ctx context.Context, namespaceID string, paths []string) error {
	return s.WithTx(ctx, func(tx *Tx) error {
		return tx.SetSparsePathsTx(ctx, namespaceID, paths)
	})
}

// SetSparsePathsTx is ReplaceSparsePathsForProject's Tx-scoped twin, callable
// inside an existing transaction — used by `devstrap add --sparse` to persist
// the initial profile in the same transaction as the project's
// project.added event and namespace_entries/git_repos upsert.
func (tx *Tx) SetSparsePathsTx(ctx context.Context, namespaceID string, paths []string) error {
	if _, err := tx.tx.ExecContext(ctx, `DELETE FROM project_sparse_paths WHERE namespace_id = ?;`, namespaceID); err != nil {
		return fmt.Errorf("clear project sparse paths: %w", err)
	}
	if len(paths) == 0 {
		return nil
	}
	now := timestampNow()
	for _, p := range paths {
		if _, err := tx.tx.ExecContext(ctx, `
INSERT INTO project_sparse_paths (namespace_id, path, created_at) VALUES (?, ?, ?);
`, namespaceID, p, now); err != nil {
			return fmt.Errorf("insert project sparse path %q: %w", p, err)
		}
	}
	return nil
}
