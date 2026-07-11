package cli

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Reederey87/DevStrap/internal/state"
)

// wckRotationPendingMetaKey / wckRotationPendingRecord alias the canonical
// marker format, which now lives in internal/state so the sync apply path can
// arm the owed-rotation marker transactionally with a synced/snapshot-imported
// trust flip (P7-SYNC-04) without importing this package. The marker records
// that a WCK rotation is OWED: a device revoke could not (or a revoke it only
// learned of remotely did not) rotate the epoch, so events keep sealing under a
// key the revoked device still holds until a local Rotate lands (issue #134).
const wckRotationPendingMetaKey = state.WCKRotationPendingMetaKey

type wckRotationPendingRecord = state.WCKRotationPendingRecord

// markWCKRotationPending persists the owed-rotation marker after a failed
// revoke-path rotation so sync's rotation gate retries it on every cycle.
func markWCKRotationPending(ctx context.Context, store *state.Store, epoch int64) error {
	raw, err := json.Marshal(wckRotationPendingRecord{Epoch: epoch, Since: time.Now().UTC()})
	if err != nil {
		return err
	}
	return store.SetLocalMeta(ctx, wckRotationPendingMetaKey, string(raw))
}

// wckRotationPendingSince reports whether a rotation is still owed. The marker
// resolves ONLY via clearWCKRotationPending after THIS device's own successful
// Rotate — never by observing a newer epoch (adversarial-review HIGH, issue
// #134): a peer that has not yet pulled the revoke can rotate for age reasons
// and grant the new epoch to the still-approved-in-its-registry revoked
// device, so "any newer epoch is active" is NOT proof the revoked device was
// excluded. A locally-run Rotate always excludes locally-revoked devices,
// which is exactly the proof the marker needs; the worst case of ignoring a
// legitimate peer rotation is one redundant epoch. A marker that fails to
// parse stays pending with a zero Since (fail-closed).
func wckRotationPendingSince(ctx context.Context, store *state.Store) (time.Time, bool, error) {
	raw, ok, err := store.GetLocalMeta(ctx, wckRotationPendingMetaKey)
	if err != nil || !ok {
		return time.Time{}, false, err
	}
	var rec wckRotationPendingRecord
	if jerr := json.Unmarshal([]byte(raw), &rec); jerr != nil {
		return time.Time{}, true, nil
	}
	return rec.Since, true, nil
}

// clearWCKRotationPending removes the marker. Called ONLY after a successful
// local Rotate (sync's owed retry, `keys rotate`, or a later revoke whose
// rotation succeeded) — every local Rotate wraps to ApprovedRecipients, which
// excludes all locally-revoked devices, satisfying the owed containment.
func clearWCKRotationPending(ctx context.Context, store *state.Store) error {
	return store.DeleteLocalMeta(ctx, wckRotationPendingMetaKey)
}
