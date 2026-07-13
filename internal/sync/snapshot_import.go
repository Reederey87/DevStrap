// snapshot_import.go applies a verified full-state snapshot to the local store
// (P4-SYNC-02, consumer half). Import is a pure last-writer-wins merge on each
// row's source-event coordinates — it writes derived namespace state directly,
// emitting NO synthetic events and fabricating no history. That makes it
// idempotent and order-independent with respect to event replay: import-then-
// replay and replay-then-import converge to the same state, because both paths
// resolve every path by the same (HLC, device_id, event_id) coordinate order.
package sync

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Reederey87/DevStrap/internal/state"
)

// retentionFloorMetaPrefix keys the local_meta row caching the highest verified
// per-device retention floor for a hub. It is machine-local (never synced) and
// only a rollback-detection hint for recovery, never authoritative state.
const retentionFloorMetaPrefix = "retention_floor:"

// RetentionFloorMetaKey is the local_meta key caching the highest verified
// per-device retention floor for a hub (P4-SYNC-02 rollback guard).
func RetentionFloorMetaKey(hubID string) string {
	return retentionFloorMetaPrefix + hubID
}

// ImportSnapshot applies a verified snapshot to the local store as a pure LWW
// merge in one transaction, then advances the per-device pull cursors to the
// floor and caches the verified floor for the recovery rollback guard.
//
// The caller (recoverFromSnapshot) has already verified the retention manifest
// signature against a locally pinned approved device, matched the fetched
// object's sha256 against the manifest, and unsealed the object under a held
// WCK, so ImportSnapshot trusts the plaintext. snapshotSHA256 is the content
// address of that sealed object, recorded with each imported anchor for audit;
// hubID keys the pull cursors advanced after the merge commits.
func ImportSnapshot(ctx context.Context, st *state.Store, snap Snapshot, snapshotSHA256, hubID string) error {
	if err := st.WithTx(ctx, func(tx *state.Tx) error {
		for _, entry := range snap.Entries {
			if err := importEntryTx(ctx, tx, entry); err != nil {
				return err
			}
		}
		for _, ts := range snap.Tombstones {
			if err := importTombstoneTx(ctx, tx, ts); err != nil {
				return err
			}
		}
		if err := importTrustTx(ctx, tx, snap.Trust); err != nil {
			return err
		}
		for _, a := range snap.Anchors {
			if err := tx.UpsertChainAnchor(ctx, a.DeviceID, a.Seq, a.ContentHash, a.FoldedHash, a.HLC, snapshotSHA256); err != nil {
				return err
			}
		}
		// Advance the local clock past the snapshot producer's HLC so subsequent
		// local events sort after everything the snapshot covers. ReceiveRemoteHLC
		// is forward-only, so a re-import is a harmless no-op.
		return tx.ReceiveRemoteHLC(ctx, snap.HLC)
	}); err != nil {
		return err
	}
	// After the merge commits, advance the per-device pull cursors to the floor
	// (forward-only) so the next incremental Pull resumes at Seq = floor and the
	// hub's retention gate is satisfied. AdvanceHubDeviceCursor never regresses a
	// higher cursor, so this is safe even when local history already ran ahead.
	for dev, floor := range snap.Floor {
		if floor <= 0 {
			continue
		}
		if err := st.AdvanceHubDeviceCursor(ctx, hubID, dev, floor-1); err != nil {
			return err
		}
	}
	return cacheRetentionFloor(ctx, st, hubID, snap.Floor)
}

// importEntryTx merges one snapshot namespace entry via pure LWW. A snapshot
// carries pre-reconciled winners, so — unlike applyEventTx's project.added path
// — no same-path/different-remote conflict logic runs; the plain coordinate
// comparison suffices.
func importEntryTx(ctx context.Context, tx *state.Tx, entry SnapshotEntry) error {
	// Skip when a dominating tombstone exists: a delete at or after this entry's
	// coordinate wins, exactly as the event apply path gates a stale add.
	if tombstoneHLC, ok, err := tx.TombstoneHLC(ctx, entry.Path); err != nil {
		return err
	} else if ok && entry.SourceEventHLC <= tombstoneHLC {
		return nil
	}
	existing, err := tx.ProjectByPath(ctx, entry.Path)
	incomingWins := true
	if err == nil {
		cur := samePathCandidate{hlc: existing.SourceEventHLC, deviceID: existing.SourceEventDeviceID, eventID: existing.SourceEventID}
		inc := samePathCandidate{hlc: entry.SourceEventHLC, deviceID: entry.SourceEventDeviceID, eventID: entry.SourceEventID}
		incomingWins = samePathLess(cur, inc)
	}
	if !incomingWins {
		// Stored project coordinates dominate → stale snapshot row for the entry
		// itself. The env pointer's coordinate is INDEPENDENT (a capture can
		// postdate the project row that carries it in the snapshot), so it still
		// merges by its own LWW compare against the surviving local row.
		if entry.Env != nil {
			return importEnvTx(ctx, tx, existing.ID, entry.Env)
		}
		return nil
	}
	ns, err := tx.UpsertProject(ctx, upsertParamsForSnapshotEntry(entry))
	if err != nil {
		return err
	}
	// Draft pointer: record the latest bundle pointer only when this entry won.
	// The snapshot's draft is the fleet-latest for the path at floor time; if a
	// newer local add dominated (handled above), its draft is newer too, so
	// importing an older pointer would regress current_snapshot_id. Idempotent on
	// source_event_id, so re-import is a no-op.
	if entry.Draft != nil {
		draftEvent := state.Event{
			ID:       entry.Draft.SourceEventID,
			DeviceID: entry.Draft.SourceEventDeviceID,
			HLC:      entry.Draft.SourceEventHLC,
		}
		if err := tx.RecordDraftSnapshotTx(ctx, ns.ID, entry.Draft.BlobRef, entry.Draft.ByteSize, entry.Draft.FileCount, draftEvent); err != nil {
			return err
		}
	}
	if entry.Env != nil {
		return importEnvTx(ctx, tx, ns.ID, entry.Env)
	}
	return nil
}

// importEnvTx merges one snapshot env-profile pointer by the same
// highest-(HLC, deviceID, eventID)-wins rule the env.profile.updated apply path
// uses (ENV-SYNC-01), synthesizing the source event from the shipped
// coordinates so UpsertEnvProfileTx stamps them and re-import stays idempotent.
func importEnvTx(ctx context.Context, tx *state.Tx, namespaceID string, env *SnapshotEnv) error {
	envEvent := state.Event{
		ID:       env.SourceEventID,
		DeviceID: env.SourceEventDeviceID,
		HLC:      env.SourceEventHLC,
	}
	hlc, deviceID, eventID, ok, err := tx.EnvProfileSourceCoords(ctx, namespaceID)
	if err != nil {
		return err
	}
	if ok && !envCoordLess(hlc, deviceID, eventID, envEvent) {
		return nil // local profile coordinates dominate → stale snapshot pointer
	}
	if _, err := tx.UpsertEnvProfileTx(ctx, namespaceID, state.EnvProfileParams{
		Name:     env.Name,
		Provider: env.Provider,
		Mode:     env.Mode,
		BlobRef:  env.BlobRef,
		VarNames: env.VarNames,
		Refs:     env.Refs,
	}, envEvent); err != nil {
		return fmt.Errorf("import env profile for namespace %s: %w", namespaceID, err)
	}
	return nil
}

// importTrustTx re-derives terminal device trust from the snapshot
// (P7-SYNC-01), mirroring the EventDeviceRevoked/EventDeviceLost apply path in
// events.go: ensure a placeholder row for an unknown target, then the
// sticky/monotonic ApplyRemoteDeviceTrustTx (only pending/approved flip; the
// local device never flips; revoked<->lost churn and re-imports are no-ops).
// needs_rotation is flagged once when any row ACTUALLY changed, so a re-import
// can never re-flag values an operator already rotated and cleared. A
// malformed row is a hard error that aborts the whole import transaction —
// the caller verified the snapshot, so malformed trust means a defective
// producer, and partial application would be worse than refusal.
//
// P7-SYNC-04: when any row actually flips, the importer also OWES a WCK
// rotation, exactly as the events.go apply path does — an EXISTING device
// recovering via snapshot past the retention floor (it already holds a key, so
// epoch>0) must mint epoch+1 excluding the revoked device rather than keep
// sealing under an epoch it still holds. A genuinely keyless fresh bootstrap
// never reaches here (recoverFromSnapshot defers before unsealing/importing
// when it holds no WCK) and is covered transitively by the granting key-holder.
// The marker is armed once, transactionally with the flips, and its storm-guard
// makes re-imports inert.
func importTrustTx(ctx context.Context, tx *state.Tx, trust []SnapshotTrust) error {
	changedAny := false
	for _, tr := range trust {
		if tr.DeviceID == "" {
			return fmt.Errorf("import snapshot trust: empty device id")
		}
		switch tr.State {
		case "revoked", "lost":
		default:
			return fmt.Errorf("import snapshot trust: unsupported state %q for device %s", tr.State, tr.DeviceID)
		}
		if err := tx.EnsureRemoteDeviceTx(ctx, tr.DeviceID); err != nil {
			return err
		}
		changed, err := tx.ApplyRemoteDeviceTrustTx(ctx, tr.DeviceID, tr.State)
		if err != nil {
			return err
		}
		changedAny = changedAny || changed
	}
	if changedAny {
		if _, err := tx.MarkEncryptedBindingsNeedingRotationTx(ctx); err != nil {
			return err
		}
		// P7-SYNC-04: owe the forward-secrecy rotation on the importer too (see
		// the events.go apply path). Guarded on epoch>0 inside the helper.
		epoch, err := tx.CurrentKeyEpochTx(ctx)
		if err != nil {
			return err
		}
		if err := tx.SetWCKRotationPendingTx(ctx, epoch); err != nil {
			return err
		}
	}
	return nil
}

// importTombstoneTx merges one surviving tombstone via LWW: a newer local add
// keeps the path; a dirty local checkout defers to a pending-delete conflict
// instead of being destroyed; otherwise the path is tombstoned so a stale add
// cannot resurrect it on a bootstrapped device.
func importTombstoneTx(ctx context.Context, tx *state.Tx, ts SnapshotTombstone) error {
	existing, err := tx.ProjectByPathKey(ctx, ts.PathKey)
	if err == nil {
		// Deliberately a bare-HLC comparison, NOT the full (HLC, device, event)
		// coordinate tie-break: the LIVE paths resolve add/delete ties by HLC
		// alone in the tombstone's favor (decideUpsert blocks an add when
		// event.HLC <= tombstoneHLC, decideDelete keeps a live row only when
		// its source HLC is STRICTLY above the delete, and tombstonePath keeps
		// the max HLC unconditionally), so both replay orders of an equal-HLC
		// add+delete converge on deleted. Import must mirror that exactly — a
		// full coordinate compare here would make import diverge from replay on
		// equal-HLC ties (reviewed and rejected as a post-review suggestion).
		if existing.SourceEventHLC > ts.TombstoneHLC {
			return nil // local add is strictly newer than the delete → keep it
		}
		if existing.DirtyState == dirtyStateDirty {
			raw, mErr := json.Marshal(pendingDeleteConflictDetails{
				Path:     existing.Path,
				EventID:  ts.SourceEventID,
				DeviceID: ts.SourceEventDeviceID,
				HLC:      ts.TombstoneHLC,
			})
			if mErr != nil {
				return mErr
			}
			return tx.InsertConflict(ctx, existing.ID, ConflictPendingDelete, string(raw))
		}
	}
	return tx.TombstoneByPathKey(ctx, ts.PathKey, ts.TombstoneHLC)
}

// upsertParamsForSnapshotEntry maps a snapshot entry to upsert params, mirroring
// upsertParamsForEvent but driving all coordinates and git metadata from the
// entry. CloneFilter/SparseConfig are not represented — like the event apply
// path, UpsertProject applies the default blobless clone filter; a bootstrapped
// device re-derives sparse config on materialization.
func upsertParamsForSnapshotEntry(entry SnapshotEntry) state.UpsertProjectParams {
	params := state.UpsertProjectParams{
		Path:                  entry.Path,
		Type:                  entry.Type,
		MaterializationPolicy: entry.MaterializationPolicy,
		SourceEventHLC:        entry.SourceEventHLC,
		SourceEventDeviceID:   entry.SourceEventDeviceID,
		SourceEventID:         entry.SourceEventID,
	}
	if entry.Git != nil {
		params.RemoteURL = entry.Git.RemoteURL
		params.RemoteKey = entry.Git.RemoteKey
		params.DefaultBranch = entry.Git.DefaultBranch
		params.LFSPolicy = entry.Git.LFSPolicy
		params.ForgeKind = entry.Git.ForgeKind
	}
	return params
}

// cacheRetentionFloor merges the snapshot's floor into the cached highest
// verified floor, monotonically per device (a hub must never walk a floor
// backward). A garbled cache is treated as empty — it is only a rollback hint.
func cacheRetentionFloor(ctx context.Context, st *state.Store, hubID string, floor Cursor) error {
	merged, err := LoadRetentionFloorCache(ctx, st, hubID)
	if err != nil {
		return err
	}
	for dev, seq := range floor {
		if seq > merged[dev] {
			merged[dev] = seq
		}
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		return fmt.Errorf("marshal retention floor cache: %w", err)
	}
	return st.SetLocalMeta(ctx, RetentionFloorMetaKey(hubID), string(raw))
}

// LoadRetentionFloorCache reads the cached highest verified per-device floor for
// a hub, or an empty map when none is cached. A garbled cache is treated as
// empty (rollback detection is a hint, never authoritative).
func LoadRetentionFloorCache(ctx context.Context, st *state.Store, hubID string) (map[string]int64, error) {
	raw, ok, err := st.GetLocalMeta(ctx, RetentionFloorMetaKey(hubID))
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	if ok {
		_ = json.Unmarshal([]byte(raw), &out)
	}
	return out, nil
}
