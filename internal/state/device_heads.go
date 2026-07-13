package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/Reederey87/DevStrap/internal/fold"
)

// DeviceHead is the persisted per-origin-device omission-detection watermark
// (P4-SYNC-05): the highest SIGNED head this device has verified from that
// origin's ack marker. PromisedFold is the origin's committed folded hash at
// PromisedSeq. OmissionPending is set the first cycle the local contiguous
// stream falls short of PromisedSeq (a one-cycle grace absorbs the in-flight
// race where a peer pulls between an origin's event push and its ack); a second
// consecutive shortfall raises the omission alarm.
type DeviceHead struct {
	DeviceID        string
	PromisedSeq     int64
	PromisedFold    string
	PromisedHLC     int64
	OmissionPending bool
}

// DeviceHead reads the recorded omission watermark for an origin device, if any.
func (s *Store) DeviceHead(ctx context.Context, deviceID string) (DeviceHead, bool, error) {
	workspaceID, err := s.WorkspaceID(ctx)
	if err != nil {
		return DeviceHead{}, false, err
	}
	var h DeviceHead
	var pending int64
	err = s.db.QueryRowContext(ctx, `
SELECT device_id, promised_seq, promised_fold, promised_hlc, omission_pending
FROM device_heads
WHERE workspace_id = ? AND device_id = ?;
`, workspaceID, deviceID).Scan(&h.DeviceID, &h.PromisedSeq, &h.PromisedFold, &h.PromisedHLC, &pending)
	if errors.Is(err, sql.ErrNoRows) {
		return DeviceHead{}, false, nil
	}
	if err != nil {
		return DeviceHead{}, false, fmt.Errorf("read device head: %w", err)
	}
	h.OmissionPending = pending != 0
	return h, true, nil
}

// UpsertDeviceHead writes the omission watermark for an origin device.
func (s *Store) UpsertDeviceHead(ctx context.Context, h DeviceHead) error {
	workspaceID, err := s.WorkspaceID(ctx)
	if err != nil {
		return err
	}
	pending := int64(0)
	if h.OmissionPending {
		pending = 1
	}
	now := timestampNow()
	_, err = s.db.ExecContext(ctx, `
INSERT INTO device_heads (workspace_id, device_id, promised_seq, promised_fold, promised_hlc, omission_pending, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(workspace_id, device_id) DO UPDATE SET
  promised_seq = excluded.promised_seq,
  promised_fold = excluded.promised_fold,
  promised_hlc = excluded.promised_hlc,
  omission_pending = excluded.omission_pending,
  updated_at = excluded.updated_at;
`, workspaceID, h.DeviceID, h.PromisedSeq, h.PromisedFold, h.PromisedHLC, pending, now)
	if err != nil {
		return fmt.Errorf("upsert device head: %w", err)
	}
	return nil
}

// DeviceGapLocallyDeclined reports whether device deviceID's contiguous event
// prefix stops at gapSeq because THIS device durably declined/deferred/
// quarantined the event occupying that slot (a LOCAL gap) rather than the hub
// withholding it (P4-SYNC-05 defect fix). Distinguishing the two prevents a
// false `withheld_tail` omission alarm against an honest hub while a peer's
// events are legitimately unreadable here (a cross-epoch key-grant grace window)
// or were consumed-quarantined (sub-epoch skew, a hash-chain break, an
// unverifiable event from a not-yet-approved device). It returns true when any
// of:
//
//   - a sync_skipped_events row names (deviceID, gapSeq) — an unclassifiable or
//     unknown-version event deferred at the hub-decrypt boundary (P6-SYNC-02);
//   - any key_grant_waits row is open — enc.v2 carriers are being deferred during
//     a cross-epoch key-grant grace window (P6-SEC-03); these are not
//     seq-attributed on the client, so any open wait suppresses the alarm
//     fail-safe (gc/compact are already refused while a wait is open);
//   - an open conflict of one of quarantineTypes names deviceID at gapSeq — or
//     carries no seq coordinate (e.g. a skew quarantine records only device_id),
//     in which case deviceID alone matches, since a consumed quarantine still
//     leaves a fold gap for that device.
//
// A gc/compact caller must NOT treat this as an omission (it is separately
// surfaced by `doctor`/`conflicts` and, for real quarantines, already blocks
// gc), so the omission alarm is suppressed for such a gap.
func (s *Store) DeviceGapLocallyDeclined(ctx context.Context, deviceID string, gapSeq int64, quarantineTypes []string) (bool, error) {
	workspaceID, err := s.WorkspaceID(ctx)
	if err != nil {
		return false, err
	}

	// (1) An event durably skipped at exactly this slot.
	var one int
	err = s.db.QueryRowContext(ctx, `
SELECT 1 FROM sync_skipped_events
WHERE workspace_id = ? AND device_id = ? AND seq = ? LIMIT 1;
`, workspaceID, deviceID, gapSeq).Scan(&one)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("check skipped event for gap: %w", err)
	}

	// (2) Any open key-grant wait: undecryptable carriers are being deferred and
	// are not seq-attributed on the client, so any open wait suppresses the alarm.
	err = s.db.QueryRowContext(ctx, `
SELECT 1 FROM key_grant_waits WHERE workspace_id = ? LIMIT 1;
`, workspaceID).Scan(&one)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("check key grant waits for gap: %w", err)
	}

	// (3) An open quarantine-class conflict naming this device at this seq (or
	// carrying no seq coordinate, e.g. a skew quarantine).
	if len(quarantineTypes) > 0 {
		placeholders := make([]string, len(quarantineTypes))
		args := make([]any, 0, len(quarantineTypes)+3)
		args = append(args, workspaceID)
		for i, t := range quarantineTypes {
			placeholders[i] = "?"
			args = append(args, t)
		}
		args = append(args, deviceID, gapSeq)
		query := `
SELECT 1 FROM conflicts
WHERE workspace_id = ? AND status = 'open'
  AND type IN (` + strings.Join(placeholders, ",") + `)
  AND json_extract(details_json, '$.device_id') = ?
  AND (json_extract(details_json, '$.seq') = ?
       OR json_extract(details_json, '$.seq') IS NULL
       OR json_extract(details_json, '$.seq') = 0)
LIMIT 1;`
		err = s.db.QueryRowContext(ctx, query, args...).Scan(&one)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("check quarantine conflict for gap: %w", err)
		}
	}
	return false, nil
}

// ResolveOmissionConflictsForDevice marks open omission conflicts of the given
// type for one origin device resolved (P4-SYNC-05 recovery path). When kind is
// non-empty only conflicts whose details name that kind are resolved (e.g. clear
// a stale withheld_tail without touching a genuine fork); an empty kind resolves
// every open omission conflict for the device. Idempotent: zero matching rows is
// a no-op. Mirrors ResolveOpenConflictsByEventID — the omission details no longer
// embed the (ever-growing) promised seq, so a stable fingerprint cannot be
// reconstructed, and matching is by device_id via json_extract.
func (s *Store) ResolveOmissionConflictsForDevice(ctx context.Context, typ, deviceID, kind, resolutionJSON string) error {
	workspaceID, err := s.WorkspaceID(ctx)
	if err != nil {
		return err
	}
	now := timestampNow()
	_, err = s.db.ExecContext(ctx, `
UPDATE conflicts SET status = 'resolved', resolution_json = ?, updated_at = ?
WHERE workspace_id = ? AND type = ? AND status = 'open'
  AND json_extract(details_json, '$.device_id') = ?
  AND (? = '' OR json_extract(details_json, '$.kind') = ?);
`, nullEmpty(resolutionJSON), now, workspaceID, typ, deviceID, kind, kind)
	if err != nil {
		return fmt.Errorf("resolve omission conflicts for device: %w", err)
	}
	return nil
}

// DeviceFold re-folds an origin device's event stream and returns the running
// fold at its highest CONTIGUOUS seq (P4-SYNC-05). The fold seeds from the
// device's imported chain anchor when one carries a folded hash (a
// snapshot-bootstrapped device holds no events below the floor), otherwise from
// FoldSeed at seq 0. Folding stops at the first seq gap, returning the fold up
// to the last contiguous event.
//
// uptoSeq > 0 bounds the walk: fold only up to that seq (used to compare
// against a peer's promised head at an exact seq). uptoSeq <= 0 folds as far as
// the contiguous prefix reaches.
//
// seeded is false when no seed can be established — no anchor fold AND the
// stream does not start at seq 1 (early events neither held nor covered by an
// anchor) — in which case the fold cannot be trusted and callers skip
// verification fail-safe. reachedSeq is the seq the returned fold commits to.
func (s *Store) DeviceFold(ctx context.Context, deviceID string, uptoSeq int64) (reachedSeq int64, foldHex string, seeded bool, err error) {
	workspaceID, err := s.WorkspaceID(ctx)
	if err != nil {
		return 0, "", false, err
	}

	// Establish the seed: prefer an imported anchor that carries a fold.
	var anchorSeq int64
	var anchorFold string
	aerr := s.db.QueryRowContext(ctx, `
SELECT anchor_seq, COALESCE(folded_hash, '')
FROM sync_chain_anchors
WHERE workspace_id = ? AND device_id = ?;
`, workspaceID, deviceID).Scan(&anchorSeq, &anchorFold)
	if aerr != nil && !errors.Is(aerr, sql.ErrNoRows) {
		return 0, "", false, fmt.Errorf("read chain anchor fold: %w", aerr)
	}

	var cur fold.State
	var curSeq int64
	haveSeed := false
	if aerr == nil && anchorFold != "" {
		decoded, ok, derr := fold.Decode(anchorFold)
		if derr != nil {
			return 0, "", false, fmt.Errorf("decode anchor fold for %s: %w", deviceID, derr)
		}
		if ok {
			cur = decoded
			curSeq = anchorSeq
			haveSeed = true
		}
	}
	if !haveSeed {
		cur = fold.Seed(workspaceID, deviceID)
		curSeq = 0
	}

	// Walk events with seq strictly above the seed, in ascending seq order.
	rows, err := s.db.QueryContext(ctx, `
SELECT seq, content_hash
FROM events
WHERE workspace_id = ? AND device_id = ? AND COALESCE(seq, 0) > ?
ORDER BY seq ASC;
`, workspaceID, deviceID, curSeq)
	if err != nil {
		return 0, "", false, fmt.Errorf("read device events for fold: %w", err)
	}
	defer func() { _ = rows.Close() }()

	sawFirst := false
	for rows.Next() {
		var seq int64
		var contentHash string
		if err := rows.Scan(&seq, &contentHash); err != nil {
			return 0, "", false, fmt.Errorf("scan device event for fold: %w", err)
		}
		if !sawFirst {
			sawFirst = true
			// With no anchor seed, the stream must start at seq 1 for the fold
			// to be trustworthy; a first seq > 1 means early events are missing
			// (not covered by an anchor) so we cannot establish a seed.
			if !haveSeed && seq != 1 {
				return 0, "", false, nil
			}
		}
		if seq != curSeq+1 {
			break // gap: stop at the last contiguous event
		}
		cur = fold.Step(cur, seq, contentHash)
		curSeq = seq
		if uptoSeq > 0 && curSeq >= uptoSeq {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return 0, "", false, fmt.Errorf("iterate device events for fold: %w", err)
	}

	if uptoSeq > 0 && curSeq < uptoSeq {
		// We could not reach the requested seq contiguously.
		return curSeq, fold.Encode(cur), haveSeed || sawFirst, nil
	}
	return curSeq, fold.Encode(cur), haveSeed || sawFirst, nil
}
