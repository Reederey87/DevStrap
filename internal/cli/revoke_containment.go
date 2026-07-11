package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
)

// revokeContainmentPendingMetaKey records device revokes whose complete local
// containment sequence has not yet finished. It is written in the same
// transaction as the trust flip and trust event, closing the crash window
// between durable distrust and best-effort containment (P7-SEC-02).
const revokeContainmentPendingMetaKey = "revoke_containment_pending"

type revokeContainmentPendingRecord struct {
	Devices map[string]time.Time `json:"devices"`
}

// markRevokeContainmentPendingTx merges a device into the machine-local
// pending set. Multiple revokes committed before sync resumes containment must
// all survive.
func markRevokeContainmentPendingTx(ctx context.Context, tx *state.Tx, deviceID string) error {
	rec := revokeContainmentPendingRecord{Devices: make(map[string]time.Time)}
	if raw, ok, err := tx.GetLocalMetaTx(ctx, revokeContainmentPendingMetaKey); err != nil {
		return err
	} else if ok {
		// A corrupt existing record must NEVER block the revoke: refusing the
		// trust flip over retry bookkeeping would keep a compromised device
		// approved — the exact wrong fail direction. Overwrite with a fresh
		// record instead. The resume actions are device-independent (bindings
		// flag + blob rewrap are global scans), so the only loss is the
		// best-effort per-device ack deletion for whatever the corrupt record
		// named — and `hub compact` reclaims revoked devices' acks anyway.
		if err := json.Unmarshal([]byte(raw), &rec); err != nil || rec.Devices == nil {
			rec = revokeContainmentPendingRecord{Devices: make(map[string]time.Time)}
		}
	}
	if _, exists := rec.Devices[deviceID]; !exists {
		rec.Devices[deviceID] = time.Now().UTC()
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return tx.SetLocalMetaTx(ctx, revokeContainmentPendingMetaKey, string(raw))
}

// revokeContainmentPending returns the pending device set. malformed is true
// when the row exists but cannot be decoded; callers must keep it pending and
// report it rather than treating corrupt retry bookkeeping as completed.
func revokeContainmentPending(ctx context.Context, store *state.Store) (devices map[string]time.Time, pending, malformed bool, err error) {
	raw, ok, err := store.GetLocalMeta(ctx, revokeContainmentPendingMetaKey)
	if err != nil || !ok {
		return nil, false, false, err
	}
	var rec revokeContainmentPendingRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil || rec.Devices == nil {
		return nil, true, true, nil
	}
	return rec.Devices, true, false, nil
}

// clearRevokeContainmentPending removes only the named devices. The row is
// deleted once the set becomes empty.
func clearRevokeContainmentPending(ctx context.Context, store *state.Store, deviceIDs ...string) error {
	remove := make(map[string]struct{}, len(deviceIDs))
	for _, id := range deviceIDs {
		remove[id] = struct{}{}
	}
	return store.WithTx(ctx, func(tx *state.Tx) error {
		raw, ok, err := tx.GetLocalMetaTx(ctx, revokeContainmentPendingMetaKey)
		if err != nil || !ok {
			return err
		}
		var rec revokeContainmentPendingRecord
		if err := json.Unmarshal([]byte(raw), &rec); err != nil || rec.Devices == nil {
			return nil // fail closed: never clear an unparseable pending row
		}
		for id := range remove {
			delete(rec.Devices, id)
		}
		if len(rec.Devices) == 0 {
			return tx.DeleteLocalMetaTx(ctx, revokeContainmentPendingMetaKey)
		}
		encoded, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return tx.SetLocalMetaTx(ctx, revokeContainmentPendingMetaKey, string(encoded))
	})
}

func sortedPendingRevokeIDs(devices map[string]time.Time) []string {
	ids := make([]string, 0, len(devices))
	for id := range devices {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// resumeRevokeContainment finishes the non-rotation portions for every pending
// revoke after this cycle has either rotated successfully or durably handed
// rotation retry ownership to wck_rotation_pending.
func resumeRevokeContainment(ctx context.Context, stdout io.Writer, opts *options, store *state.Store, hub dssync.Hub, rotationAccounted bool) error {
	devices, pending, malformed, err := revokeContainmentPending(ctx, store)
	if err != nil || !pending {
		return err
	}
	if !rotationAccounted {
		// The rotation half of containment hasn't run/been owed this cycle;
		// don't do the rest yet. (A malformed marker still opened the rotation
		// gate, so a rotation was attempted — this branch is the not-yet case.)
		return nil
	}
	if _, err := store.MarkEncryptedBindingsNeedingRotation(ctx); err != nil {
		_, _ = fmt.Fprintf(stdout, "warning: pending revoke containment: flagging secret bindings failed (retried next sync): %v\n", err)
		return nil
	}
	if _, err := rewrapBlobsOnRevoke(ctx, store, opts, hub); err != nil {
		_, _ = fmt.Fprintf(stdout, "warning: pending revoke blob re-encryption incomplete: %v\n", err)
		return nil
	}
	if malformed {
		// A malformed record names no devices, but the rotation that just ran
		// (or is durably owed) excludes EVERY locally-revoked device — so the
		// only containment left is per-device ack deletion, which we cannot
		// target and which `hub compact` reclaims anyway. Clear the whole row
		// so the rotation gate does not fire on every subsequent sync (a
		// storm); the global containment above has run. Fail-closed only on
		// the read side (never treat a corrupt marker as "nothing pending");
		// here containment is proven done, so clearing is correct.
		if cerr := store.DeleteLocalMeta(ctx, revokeContainmentPendingMetaKey); cerr != nil {
			return cerr
		}
		_, _ = fmt.Fprintln(stdout, "Recovered a malformed revoke-containment marker: rotation + secret flags + blob rewrap re-run; cleared (per-device ack cleanup deferred to hub compact)")
		return nil
	}
	ids := sortedPendingRevokeIDs(devices)
	if hub != nil {
		for _, id := range ids {
			if err := hub.DeleteAck(ctx, id); err != nil {
				_, _ = fmt.Fprintf(stdout, "warning: failed to delete %s's sync ack from the hub: %v\n", id, err)
			}
		}
	}
	if err := clearRevokeContainmentPending(ctx, store, ids...); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "Resumed revoke containment for %d device(s)\n", len(ids))
	return nil
}
