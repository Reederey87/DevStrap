package state

import (
	"context"
	"fmt"
)

// DeviceWip is one device's last-pushed working-state validation plane Layer
// B WIP ref for a project (repo.wip.pushed). The row is a live mirror, not a
// history log: a later push overwrites the previous one for the same
// (device, project).
type DeviceWip struct {
	DeviceID      string
	Path          string
	PathKey       string
	Ref           string
	SHA           string
	BaseSHA       string
	CapturedAt    string
	ObservedAtHLC int64
	SourceEventID string
	UpdatedAt     string
}

// WipParams carries the observed fields of a repo.wip.pushed event. It is
// decoupled from the sync package's event payload type to avoid an import
// cycle (internal/sync already imports internal/state), mirroring
// GitstateParams.
type WipParams struct {
	Ref        string
	SHA        string
	BaseSHA    string
	CapturedAt string
}

// UpsertDeviceWipTx mirrors a device's last-pushed WIP ref for one project
// (working-state validation plane Layer B). Apply is MIRROR-ONLY: this
// overwrites the existing row for (device_id, path_key) rather than
// appending, since the table holds "current state as last observed," not a
// history log. The update is skipped when an equal-or-newer observation is
// already recorded, so an out-of-order redelivery cannot regress the mirror.
// No FK to devices or namespace_entries — see migration 00030's comment.
func (tx *Tx) UpsertDeviceWipTx(ctx context.Context, deviceID, pathKey, path string, p WipParams, event Event) error {
	now := timestampNow()
	if _, err := tx.tx.ExecContext(ctx, `
INSERT INTO device_wip (
  workspace_id, device_id, path_key, path, ref, sha, base_sha, captured_at,
  observed_at_hlc, source_event_id, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(workspace_id, device_id, path_key) DO UPDATE SET
  path = excluded.path,
  ref = excluded.ref,
  sha = excluded.sha,
  base_sha = excluded.base_sha,
  captured_at = excluded.captured_at,
  observed_at_hlc = excluded.observed_at_hlc,
  source_event_id = excluded.source_event_id,
  updated_at = excluded.updated_at
WHERE excluded.observed_at_hlc >= device_wip.observed_at_hlc;
`, tx.workspaceID, deviceID, pathKey, path, p.Ref, p.SHA, p.BaseSHA, p.CapturedAt,
		event.HLC, event.ID, now); err != nil {
		return fmt.Errorf("upsert device wip: %w", err)
	}
	return nil
}

// DeleteDeviceWip removes one device's pending-WIP mirror row for a project
// after `wip drop` deletes the corresponding remote ref. This clears only the
// LOCAL mirror on the device that ran the drop — it does not propagate to
// other devices' mirrors (no repo.wip.dropped event exists; automatic
// fleet-wide WIP-ref GC is explicitly out of scope for this feature, see
// spec/07). No-op (no error) if no matching row exists.
func (s *Store) DeleteDeviceWip(ctx context.Context, deviceID, pathKey string) error {
	workspaceID, err := s.WorkspaceID(ctx)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM device_wip WHERE workspace_id = ? AND device_id = ? AND path_key = ?;`, workspaceID, deviceID, pathKey)
	if err != nil {
		return fmt.Errorf("delete device wip: %w", err)
	}
	return nil
}

// DeviceWipForProject reads every device's last-pushed WIP ref for a project,
// newest push first. This is the read side backing a future `wip status` CLI
// surfacing (out of scope for this change).
func (s *Store) DeviceWipForProject(ctx context.Context, pathKey string) ([]DeviceWip, error) {
	workspaceID, err := s.WorkspaceID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.reader().QueryContext(ctx, `
SELECT device_id, path, path_key, ref, sha, base_sha, captured_at,
       observed_at_hlc, source_event_id, updated_at
FROM device_wip
WHERE workspace_id = ? AND path_key = ?
ORDER BY observed_at_hlc DESC;
`, workspaceID, pathKey)
	if err != nil {
		return nil, fmt.Errorf("read device wip for project: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []DeviceWip
	for rows.Next() {
		var w DeviceWip
		if err := rows.Scan(&w.DeviceID, &w.Path, &w.PathKey, &w.Ref, &w.SHA, &w.BaseSHA, &w.CapturedAt,
			&w.ObservedAtHLC, &w.SourceEventID, &w.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan device wip: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate device wip: %w", err)
	}
	return out, nil
}
