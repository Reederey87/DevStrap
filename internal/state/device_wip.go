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

// TombstoneDeviceWipTx records a deletion without erasing the last observed
// push. Keeping that push's sha is required by the read predicate: a racing
// push from another device stream can have a lower/equal HLC but a different
// sha, and therefore remains live.
//
// droppedSHA is the sha the delete was LEASED against, or "" when the deleting
// command found the ref already absent and so proved nothing about which sha
// was there. An empty dropped_sha makes the tombstone sha-AGNOSTIC in the
// predicate below — observed absence outranks any sha guess.
//
// dropped_at_hlc and dropped_sha must move TOGETHER. Taking MAX of the HLC
// while letting the sha be last-write-wins would converge to a (hlc, sha) pair
// that no drop event ever carried: with two honest drops of different shas
// arriving newest-first, the row would end up holding the newer HLC beside the
// older drop's sha, and a push of the sha that WAS deleted would then read
// live forever. The CASE keeps the pair coherent, with a deterministic
// sha-order tiebreak so every device converges on the same winner when two
// drops share an HLC.
func (tx *Tx) TombstoneDeviceWipTx(ctx context.Context, deviceID, pathKey, path, droppedSHA string, event Event) error {
	now := timestampNow()
	if _, err := tx.tx.ExecContext(ctx, `
INSERT INTO device_wip (
  workspace_id, device_id, path_key, path, ref, sha, base_sha, captured_at,
  observed_at_hlc, source_event_id, updated_at, dropped_at_hlc, dropped_sha
) VALUES (?, ?, ?, ?, '', '', '', '', 0, ?, ?, ?, ?)
ON CONFLICT(workspace_id, device_id, path_key) DO UPDATE SET
  dropped_sha = CASE
    WHEN excluded.dropped_at_hlc > COALESCE(device_wip.dropped_at_hlc, 0)
      OR (excluded.dropped_at_hlc = COALESCE(device_wip.dropped_at_hlc, 0)
          AND excluded.dropped_sha < COALESCE(device_wip.dropped_sha, ''))
    THEN excluded.dropped_sha
    ELSE device_wip.dropped_sha END,
  dropped_at_hlc = MAX(COALESCE(device_wip.dropped_at_hlc, 0), excluded.dropped_at_hlc),
  updated_at = excluded.updated_at;
`, tx.workspaceID, deviceID, pathKey, path, event.ID, now, event.HLC, droppedSHA); err != nil {
		return fmt.Errorf("tombstone device wip: %w", err)
	}
	return nil
}

// deviceWipLivePredicate deliberately includes sha inequality, not just HLC.
// Push and drop events can arrive from different device streams in either
// order. A LEASED drop proves only that it deleted dropped_sha; if the owner
// concurrently pushed a different sha, that recovery ref is live even when its
// bare int64 HLC is lower than or equal to the dropper's HLC. This also covers
// equal HLCs, which have no device-id tiebreak in this mirror.
//
// Two guards keep that escape from firing where it must not:
//
//   - observed_at_hlc > 0. A drop arriving with no prior push inserts a
//     TOMBSTONE-ONLY row (observed_at_hlc = 0, empty ref/sha) — a routine
//     delivery order, and the normal state on a snapshot-bootstrapped device
//     whose pushed event was already compacted away. Ungated, that row's empty
//     sha differs from dropped_sha and the tombstone would read LIVE: a phantom
//     pending-WIP entry with no ref, surfaced by `wip status`,
//     `status --all-devices`, and `doctor`, and unusable by `wip show`/`apply`.
//     An unobserved push cannot be the racing push the escape protects.
//
//   - dropped_sha <> ”. An empty dropped_sha means the dropping command found
//     the ref ALREADY ABSENT and so leased nothing — it proved the ref is gone
//     but not which sha was there. Observed absence must therefore outrank any
//     sha comparison, or a stale-mirror drop would emit a sha that was never
//     deleted and resurrect the row on every device as a permanent phantom.
const deviceWipLivePredicate = `(dropped_at_hlc IS NULL
   OR observed_at_hlc > dropped_at_hlc
   OR (observed_at_hlc > 0
       AND COALESCE(dropped_sha, '') <> ''
       AND sha <> dropped_sha))`

// DeviceWipForProject reads every device's last-pushed WIP ref for a project,
// newest push first. This is the read side backing every consumer of the
// mirror: `wip status`/`show`/`fetch`/`apply`/`drop` (which resolve and verify
// a peer's ref against it) and the pending-WIP rows in `status --all-devices`
// and `doctor`.
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
  AND `+deviceWipLivePredicate+`
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

// DeviceWipAll reads every live WIP mirror row workspace-wide. Age filtering
// happens in Go and cardinality is only devices x projects (normally tens to
// hundreds), so observed_at_hlc deliberately has no dedicated index.
func (s *Store) DeviceWipAll(ctx context.Context) ([]DeviceWip, error) {
	workspaceID, err := s.WorkspaceID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.reader().QueryContext(ctx, `
SELECT device_id, path, path_key, ref, sha, base_sha, captured_at,
       observed_at_hlc, source_event_id, updated_at
FROM device_wip
WHERE workspace_id = ? AND `+deviceWipLivePredicate+`
ORDER BY path_key, observed_at_hlc DESC;
`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("read all device wip: %w", err)
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
