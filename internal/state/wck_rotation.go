package state

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// WCKRotationPendingMetaKey is the local_meta row recording that a WCK rotation
// is OWED: a device learned of a `devices revoke`/`lost` (its own failed
// revoke-path rotation, or a synced/snapshot-imported trust flip) and must mint
// epoch+1 that excludes the revoked device before the fleet stops sealing under
// a key the revoked device still holds (issue #134, P7-SYNC-04). Machine-local
// retry bookkeeping only — it never rides the event log.
//
// The format is defined here (not in internal/cli) so the sync apply path can
// arm the marker transactionally with the trust flip without importing the cli
// package; the cli helpers alias these identifiers.
const WCKRotationPendingMetaKey = "wck_rotation_pending"

// WCKRotationPendingRecord is the JSON value of the pending row. Epoch records
// the epoch that was ACTIVE when the rotation was owed (diagnostic only — a
// newer epoch alone is NOT proof the revoked device was excluded; only a local
// Rotate is, so the marker resolves solely via its explicit delete). Since is
// the "owed since" timestamp surfaced by `doctor` and sync warnings.
type WCKRotationPendingRecord struct {
	Epoch int64     `json:"epoch"`
	Since time.Time `json:"since"`
}

// CurrentKeyEpochTx is the transactional form of CurrentKeyEpoch: the highest
// WCK epoch this device holds a key for (0 if none). Callers arming the
// owed-rotation marker read it inside the same transaction as the trust flip so
// a keyless device (epoch 0) never arms a rotation it cannot perform.
func (tx *Tx) CurrentKeyEpochTx(ctx context.Context) (int64, error) {
	var epoch int64
	if err := tx.tx.QueryRowContext(ctx, `
SELECT COALESCE(MAX(epoch), 0) FROM workspace_keys WHERE workspace_id = ?;
`, tx.workspaceID).Scan(&epoch); err != nil {
		return 0, fmt.Errorf("read current key epoch: %w", err)
	}
	return epoch, nil
}

// SetWCKRotationPendingTx arms the owed-rotation marker transactionally, atomic
// with the trust flip that owes it (P7-SYNC-04). It makes the receiver's next
// sync rotation gate (maybeRotateWorkspaceKey) mint epoch+1 excluding the
// just-revoked device, so a fleet device that only LEARNS of a revocation
// (via a synced device.revoked/lost event or a snapshot import) — not just the
// device that ran the revoke — still owes the forward-secrecy rotation.
//
// epoch is the active epoch at flip time; epoch<=0 is a no-op because a keyless
// device holds no key the revoked device could read AND its rotation gate skips
// epoch 0, which would otherwise strand the marker unresolved forever.
//
// Storm-guard: an existing marker (parseable or not) is left UNTOUCHED so a
// later revoke flip cannot reset the "owed since" clock and replayed/re-imported
// flips are inert. A malformed existing marker therefore stays pending, matching
// the cli reader's fail-closed treatment.
func (tx *Tx) SetWCKRotationPendingTx(ctx context.Context, epoch int64) error {
	if epoch <= 0 {
		return nil
	}
	if _, ok, err := tx.GetLocalMetaTx(ctx, WCKRotationPendingMetaKey); err != nil {
		return err
	} else if ok {
		return nil
	}
	raw, err := json.Marshal(WCKRotationPendingRecord{Epoch: epoch, Since: time.Now().UTC()})
	if err != nil {
		return err
	}
	return tx.SetLocalMetaTx(ctx, WCKRotationPendingMetaKey, string(raw))
}
