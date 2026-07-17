package state

import (
	"context"
	"fmt"
)

// DeviceGitstate is one device's last-observed working-state snapshot for a
// project (working-state validation plane Layer A, repo.gitstate.observed).
// The row is a live mirror, not a history log: a later observation overwrites
// the previous one for the same (device, project).
type DeviceGitstate struct {
	DeviceID       string
	Path           string
	PathKey        string
	Branch         string
	HeadSHA        string
	UpstreamBranch string
	UpstreamSHA    string
	DirtyCount     int
	UntrackedCount int
	UnmergedCount  int
	AheadCount     int
	BehindCount    int
	StashCount     int
	ObservedAtHLC  int64
	SourceEventID  string
	UpdatedAt      string
}

// GitstateParams carries the observed fields of a repo.gitstate.observed
// event. It is decoupled from the sync package's event payload type to avoid
// an import cycle (internal/sync already imports internal/state), mirroring
// EnvProfileParams.
type GitstateParams struct {
	Branch         string
	HeadSHA        string
	UpstreamBranch string
	UpstreamSHA    string
	DirtyCount     int
	UntrackedCount int
	UnmergedCount  int
	AheadCount     int
	BehindCount    int
	StashCount     int
}

// UpsertDeviceGitstateTx mirrors a device's observed git working-state for one
// project (working-state validation plane Layer A). Apply is MIRROR-ONLY:
// this overwrites the existing row for (device_id, path_key) rather than
// appending, since the table holds "current state as last observed," not a
// history log. The update is skipped when an equal-or-newer observation is
// already recorded, so an out-of-order redelivery cannot regress the mirror.
// No FK to devices or namespace_entries — see migration 00029's comment.
func (tx *Tx) UpsertDeviceGitstateTx(ctx context.Context, deviceID, pathKey, path string, p GitstateParams, event Event) error {
	now := timestampNow()
	if _, err := tx.tx.ExecContext(ctx, `
INSERT INTO device_gitstate (
  workspace_id, device_id, path_key, path, branch, head_sha, upstream_branch, upstream_sha,
  dirty_count, untracked_count, unmerged_count, ahead_count, behind_count, stash_count,
  observed_at_hlc, source_event_id, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(workspace_id, device_id, path_key) DO UPDATE SET
  path = excluded.path,
  branch = excluded.branch,
  head_sha = excluded.head_sha,
  upstream_branch = excluded.upstream_branch,
  upstream_sha = excluded.upstream_sha,
  dirty_count = excluded.dirty_count,
  untracked_count = excluded.untracked_count,
  unmerged_count = excluded.unmerged_count,
  ahead_count = excluded.ahead_count,
  behind_count = excluded.behind_count,
  stash_count = excluded.stash_count,
  observed_at_hlc = excluded.observed_at_hlc,
  source_event_id = excluded.source_event_id,
  updated_at = excluded.updated_at
WHERE excluded.observed_at_hlc >= device_gitstate.observed_at_hlc;
`, tx.workspaceID, deviceID, pathKey, path, p.Branch, p.HeadSHA, p.UpstreamBranch, p.UpstreamSHA,
		p.DirtyCount, p.UntrackedCount, p.UnmergedCount, p.AheadCount, p.BehindCount, p.StashCount,
		event.HLC, event.ID, now); err != nil {
		return fmt.Errorf("upsert device gitstate: %w", err)
	}
	return nil
}

// DeviceGitstateForProject reads every device's last-observed working-state
// for a project, newest observation first. This is the read side backing the
// future `status --all-devices`/`doctor` CLI surfacing (out of scope for this
// change).
func (s *Store) DeviceGitstateForProject(ctx context.Context, pathKey string) ([]DeviceGitstate, error) {
	workspaceID, err := s.WorkspaceID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.reader().QueryContext(ctx, `
SELECT device_id, path, path_key, branch, head_sha, upstream_branch, upstream_sha,
       dirty_count, untracked_count, unmerged_count, ahead_count, behind_count, stash_count,
       observed_at_hlc, source_event_id, updated_at
FROM device_gitstate
WHERE workspace_id = ? AND path_key = ?
ORDER BY observed_at_hlc DESC;
`, workspaceID, pathKey)
	if err != nil {
		return nil, fmt.Errorf("read device gitstate for project: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []DeviceGitstate
	for rows.Next() {
		var g DeviceGitstate
		if err := rows.Scan(&g.DeviceID, &g.Path, &g.PathKey, &g.Branch, &g.HeadSHA, &g.UpstreamBranch, &g.UpstreamSHA,
			&g.DirtyCount, &g.UntrackedCount, &g.UnmergedCount, &g.AheadCount, &g.BehindCount, &g.StashCount,
			&g.ObservedAtHLC, &g.SourceEventID, &g.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan device gitstate: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate device gitstate: %w", err)
	}
	return out, nil
}
