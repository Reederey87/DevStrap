// snapshot_reads.go holds the producer-side reads for full-state snapshot
// production (P4-HUB-11, `devstrap hub compact`): the derived namespace map,
// surviving tombstones, per-device chain anchors, and the current local HLC.
// They mirror the wire shapes in internal/sync/snapshot.go but live in state to
// avoid an import cycle (sync imports state); internal/sync.BuildSnapshot maps
// them across.
package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
)

// SnapshotEntry is one active namespace-map row with its source-event
// coordinates plus optional git/draft sub-objects, read for snapshot
// production.
type SnapshotEntry struct {
	Path                  string
	PathKey               string
	Type                  string
	DisplayName           string
	MaterializationPolicy string
	Status                string
	SourceEventHLC        int64
	SourceEventDeviceID   string
	SourceEventID         string
	Git                   *SnapshotEntryGit
	Draft                 *SnapshotEntryDraft
	Env                   *SnapshotEntryEnv
}

// SnapshotEntryGit mirrors the git_repos row attached to a namespace entry.
// CloneFilter/SparseConfig are deliberately omitted: like the event apply path,
// import re-derives the default blobless filter and a bootstrapped device
// re-derives sparse config on materialization.
type SnapshotEntryGit struct {
	RemoteURL     string
	RemoteKey     string
	DefaultBranch string
	LFSPolicy     string
	ForgeKind     string
}

// SnapshotEntryDraft is the latest draft-bundle pointer for a draft project.
type SnapshotEntryDraft struct {
	BlobRef             string
	ByteSize            int64
	FileCount           int64
	SourceEventHLC      int64
	SourceEventDeviceID string
	SourceEventID       string
}

// SnapshotEntryEnv is the env-profile pointer attached to a namespace entry
// (ENV-SYNC-01). A snapshot ships it so env profiles survive event-log
// compaction: without it, a snapshot-bootstrapped device would never learn a
// profile whose env.profile.updated carrier was compacted away.
type SnapshotEntryEnv struct {
	Name                string
	Provider            string
	Mode                string
	BlobRef             string
	VarNames            []string
	Refs                map[string]string
	SourceEventHLC      int64
	SourceEventDeviceID string
	SourceEventID       string
}

// SnapshotTombstone is one surviving deleted entry (kept so a stale add cannot
// resurrect a deleted path on a bootstrapped device).
type SnapshotTombstone struct {
	PathKey             string
	TombstoneHLC        int64
	SourceEventDeviceID string
	SourceEventID       string
}

// SnapshotTrustRow is one terminal device-trust row (revoked/lost) read for
// snapshot production (P7-SYNC-01): compaction deletes the device.revoked/
// device.lost event, so the snapshot must carry the derived terminal state or
// a snapshot-bootstrapped device keeps the revoked device approved forever.
type SnapshotTrustRow struct {
	DeviceID   string
	TrustState string
}

// SnapshotChainAnchor is one origin device's hash-chain anchor for the snapshot:
// the content hash of the last event the snapshot covers for that device (at
// seq = floor-1).
type SnapshotChainAnchor struct {
	DeviceID    string
	Seq         int64
	ContentHash string
	HLC         int64
}

// SnapshotEntries returns every active namespace entry with its git_repos row
// and latest draft pointer, carrying the source-event coordinates that make
// import a pure LWW merge (P4-HUB-11). Entries are returned in path order for a
// deterministic snapshot document.
func (s *Store) SnapshotEntries(ctx context.Context) ([]SnapshotEntry, error) {
	projects, err := s.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]SnapshotEntry, 0, len(projects))
	for _, p := range projects {
		entry := SnapshotEntry{
			Path:                  p.Path,
			PathKey:               p.PathKey,
			Type:                  p.Type,
			DisplayName:           p.DisplayName,
			MaterializationPolicy: p.MaterializationPolicy,
			Status:                p.Status,
			SourceEventHLC:        p.SourceEventHLC,
			SourceEventDeviceID:   p.SourceEventDeviceID,
			SourceEventID:         p.SourceEventID,
		}
		if p.RemoteURL != "" {
			entry.Git = &SnapshotEntryGit{
				RemoteURL:     p.RemoteURL,
				RemoteKey:     p.RemoteKey,
				DefaultBranch: p.DefaultBranch,
				LFSPolicy:     p.LFSPolicy,
				ForgeKind:     p.ForgeKind,
			}
		}
		draft, err := s.LatestDraftSnapshot(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		if draft != nil {
			entry.Draft = &SnapshotEntryDraft{
				BlobRef:             draft.BlobRef,
				ByteSize:            draft.ByteSize,
				FileCount:           draft.FileCount,
				SourceEventHLC:      draft.SourceEventHLC,
				SourceEventDeviceID: draft.SourceEventDeviceID,
				SourceEventID:       draft.SourceEventID,
			}
		}
		env, err := s.snapshotEnvForProject(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		entry.Env = env
		out = append(out, entry)
	}
	return out, nil
}

// snapshotEnvForProject reads the active env-profile pointer for one namespace
// entry (ENV-SYNC-01). Profiles without source-event coordinates are skipped:
// they predate the exchange (or came through the legacy no-event wrappers), so
// they never synced anyway and a re-capture is what stamps them — shipping them
// with zero coordinates would let any later event permanently dominate them on
// importers while pretending they had a place in the LWW order.
func (s *Store) snapshotEnvForProject(ctx context.Context, namespaceID string) (*SnapshotEntryEnv, error) {
	var env SnapshotEntryEnv
	var profileID string
	var nHLC sql.NullInt64
	var nDev, nEvt sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT e.id, e.name, e.provider, e.mode, e.source_event_hlc, e.source_event_device_id, e.source_event_id
FROM namespace_entries n
JOIN env_profiles e ON e.id = n.env_profile_id
WHERE n.id = ? AND n.status = 'active';
`, namespaceID).Scan(&profileID, &env.Name, &env.Provider, &env.Mode, &nHLC, &nDev, &nEvt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("read snapshot env profile: %w", err)
	}
	if !nHLC.Valid || !nDev.Valid || nDev.String == "" || !nEvt.Valid || nEvt.String == "" {
		return nil, nil // never synced; see doc comment
	}
	env.SourceEventHLC = nHLC.Int64
	env.SourceEventDeviceID = nDev.String
	env.SourceEventID = nEvt.String
	rows, err := s.db.QueryContext(ctx, `
SELECT var_name, COALESCE(encrypted_value_ref, ''), COALESCE(provider_ref, '')
FROM secret_bindings
WHERE env_profile_id = ?
ORDER BY var_name;
`, profileID)
	if err != nil {
		return nil, fmt.Errorf("read snapshot env bindings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var varName, encRef, provRef string
		if err := rows.Scan(&varName, &encRef, &provRef); err != nil {
			return nil, fmt.Errorf("scan snapshot env binding: %w", err)
		}
		if encRef != "" {
			env.BlobRef = encRef
			env.VarNames = append(env.VarNames, varName)
		} else {
			if env.Refs == nil {
				env.Refs = make(map[string]string)
			}
			env.Refs[varName] = provRef
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &env, nil
}

// SnapshotTombstones returns every deleted namespace entry that carries a
// tombstone HLC, ordered by path_key (P4-HUB-11). A snapshot ships these so a
// bootstrapped device blocks a stale add from resurrecting a deleted path.
func (s *Store) SnapshotTombstones(ctx context.Context) ([]SnapshotTombstone, error) {
	workspaceID, err := s.WorkspaceID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT path_key, COALESCE(tombstone_hlc, 0), COALESCE(source_event_device_id, ''), COALESCE(source_event_id, '')
FROM namespace_entries
WHERE workspace_id = ? AND status = 'deleted' AND tombstone_hlc IS NOT NULL
ORDER BY path_key;
`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("read snapshot tombstones: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SnapshotTombstone
	for rows.Next() {
		var ts SnapshotTombstone
		if err := rows.Scan(&ts.PathKey, &ts.TombstoneHLC, &ts.SourceEventDeviceID, &ts.SourceEventID); err != nil {
			return nil, fmt.Errorf("scan snapshot tombstone: %w", err)
		}
		out = append(out, ts)
	}
	return out, rows.Err()
}

// SnapshotTrust returns every device row in a TERMINAL trust state
// (revoked/lost) in deterministic id order (P7-SYNC-01). pending/approved rows
// are excluded by design — approval re-grants ride as fresh events above the
// floor — and the local device can never match (its state is 'local').
func (s *Store) SnapshotTrust(ctx context.Context) ([]SnapshotTrustRow, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, trust_state FROM devices
WHERE trust_state IN ('revoked', 'lost')
ORDER BY id;
`)
	if err != nil {
		return nil, fmt.Errorf("read snapshot device trust: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SnapshotTrustRow
	for rows.Next() {
		var tr SnapshotTrustRow
		if err := rows.Scan(&tr.DeviceID, &tr.TrustState); err != nil {
			return nil, fmt.Errorf("scan snapshot device trust: %w", err)
		}
		out = append(out, tr)
	}
	return out, rows.Err()
}

// ChainAnchorsForFloors returns, for each device in floors, the hash-chain
// anchor a snapshot-bootstrapped device needs to verify that device's first
// post-floor event (P4-HUB-11): the content hash of the event at seq = floor-1.
// It reads the local events table first; when that row is absent — possible only
// for a device this machine itself imported via snapshot, so it holds no events
// below the floor — it falls back to the existing sync_chain_anchors row.
// Devices with floor <= 1 (nothing below the floor) and devices for which
// neither source exists are skipped. Deterministic device order.
func (s *Store) ChainAnchorsForFloors(ctx context.Context, floors map[string]int64) ([]SnapshotChainAnchor, error) {
	workspaceID, err := s.WorkspaceID(ctx)
	if err != nil {
		return nil, err
	}
	devices := make([]string, 0, len(floors))
	for dev := range floors {
		devices = append(devices, dev)
	}
	sort.Strings(devices)
	var out []SnapshotChainAnchor
	for _, dev := range devices {
		anchorSeq := floors[dev] - 1
		if anchorSeq < 1 {
			continue // floor covers no event for this device
		}
		var contentHash string
		var hlc int64
		err := s.db.QueryRowContext(ctx, `
SELECT COALESCE(content_hash, ''), COALESCE(hlc, 0) FROM events
WHERE workspace_id = ? AND device_id = ? AND seq = ?;
`, workspaceID, dev, anchorSeq).Scan(&contentHash, &hlc)
		switch {
		case err == nil:
			out = append(out, SnapshotChainAnchor{DeviceID: dev, Seq: anchorSeq, ContentHash: contentHash, HLC: hlc})
			continue
		case !errors.Is(err, sql.ErrNoRows):
			return nil, fmt.Errorf("read chain anchor event for %s: %w", dev, err)
		}
		// Fallback: a device we imported via snapshot has no event row below the
		// floor; reuse the anchor row imported with that snapshot.
		var aSeq, aHLC int64
		var aHash string
		err = s.db.QueryRowContext(ctx, `
SELECT anchor_seq, anchor_content_hash, anchor_hlc FROM sync_chain_anchors
WHERE workspace_id = ? AND device_id = ?;
`, workspaceID, dev).Scan(&aSeq, &aHash, &aHLC)
		switch {
		case err == nil:
			out = append(out, SnapshotChainAnchor{DeviceID: dev, Seq: aSeq, ContentHash: aHash, HLC: aHLC})
		case errors.Is(err, sql.ErrNoRows):
			// Neither an event row nor an imported anchor — skip.
		default:
			return nil, fmt.Errorf("read chain anchor fallback for %s: %w", dev, err)
		}
	}
	return out, nil
}

// CurrentHLC returns the local device's current HLC clock without minting an
// event (P4-HUB-11): the max of the recorded device clock and the highest event
// HLC held in the workspace. A produced snapshot is stamped with this so that,
// after an importer applies ReceiveRemoteHLC(snap.HLC), every later local event
// sorts after everything the snapshot covers.
func (s *Store) CurrentHLC(ctx context.Context) (int64, error) {
	workspaceID, err := s.WorkspaceID(ctx)
	if err != nil {
		return 0, err
	}
	device, err := s.CurrentDevice(ctx)
	if err != nil {
		return 0, err
	}
	var eventsMax int64
	if err := s.db.QueryRowContext(ctx, `
SELECT COALESCE(MAX(hlc), 0) FROM events WHERE workspace_id = ?;
`, workspaceID).Scan(&eventsMax); err != nil {
		return 0, fmt.Errorf("read max event hlc: %w", err)
	}
	var deviceClock int64
	if err := s.db.QueryRowContext(ctx, `
SELECT COALESCE(MAX(last_hlc), 0) FROM device_sync_state WHERE device_id = ?;
`, device.ID).Scan(&deviceClock); err != nil {
		return 0, fmt.Errorf("read device clock: %w", err)
	}
	if deviceClock > eventsMax {
		return deviceClock, nil
	}
	return eventsMax, nil
}
