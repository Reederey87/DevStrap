// wck_rotation_owed_test.go pins P7-SYNC-04: a device that LEARNS of a
// revocation — via a synced device.revoked/lost event or a snapshot import —
// owes the forward-secrecy WCK rotation, not only the device that ran the
// revoke. The owed marker is armed transactionally with the trust flip, guarded
// on epoch>0, and its storm-guard makes replays/re-imports inert.
package sync

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/state"
)

// wckRotationOwed reads the P7-SYNC-04 owed-rotation marker as the cli rotation
// gate does, returning the parsed record and whether it is set.
func wckRotationOwed(t *testing.T, st *state.Store) (state.WCKRotationPendingRecord, bool) {
	t.Helper()
	raw, ok, err := st.GetLocalMeta(context.Background(), state.WCKRotationPendingMetaKey)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return state.WCKRotationPendingRecord{}, false
	}
	var rec state.WCKRotationPendingRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		t.Fatalf("owed marker %q does not parse: %v", raw, err)
	}
	return rec, true
}

// seedKeyEpoch gives the local device a held WCK at the given epoch so the
// owed-rotation marker's epoch>0 guard is satisfied.
func seedKeyEpoch(t *testing.T, st *state.Store, epoch int64) {
	t.Helper()
	if err := st.RecordKeyEpoch(context.Background(), epoch, "kid-test", "self"); err != nil {
		t.Fatal(err)
	}
}

// TestRemoteRevokeApplyOwesRotation: applying a synced device.revoked flips
// trust AND arms the owed-rotation marker at the active epoch, so the receiver's
// next sync rotation gate mints epoch+1 excluding the revoked device.
func TestRemoteRevokeApplyOwesRotation(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	signingA := addRemoteDeviceForApplyTest(t, st, "device-a", "approved")
	addRemoteDeviceForApplyTest(t, st, "device-b", "approved")
	seedKeyEpoch(t, st, 3)
	now := time.Now().UnixMilli()

	revoke := signedDeviceTrustEvent(t, signingA, "evt_revoke_b", "device-a", 1, now, EventDeviceRevoked, "device-b")
	if _, err := ApplyEvents(ctx, st, []state.Event{revoke}); err != nil {
		t.Fatal(err)
	}
	rec, ok := wckRotationOwed(t, st)
	if !ok {
		t.Fatal("remote revoke apply did not arm the owed-rotation marker")
	}
	if rec.Epoch != 3 {
		t.Fatalf("owed marker epoch=%d, want 3 (active epoch at flip)", rec.Epoch)
	}
	if rec.Since.IsZero() {
		t.Fatal("owed marker Since is zero, want the flip timestamp")
	}
}

// TestRemoteLostApplyOwesRotation: device.lost owes the rotation too.
func TestRemoteLostApplyOwesRotation(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	signingA := addRemoteDeviceForApplyTest(t, st, "device-a", "approved")
	addRemoteDeviceForApplyTest(t, st, "device-b", "approved")
	seedKeyEpoch(t, st, 1)
	now := time.Now().UnixMilli()

	lost := signedDeviceTrustEvent(t, signingA, "evt_lost_b", "device-a", 1, now, EventDeviceLost, "device-b")
	if _, err := ApplyEvents(ctx, st, []state.Event{lost}); err != nil {
		t.Fatal(err)
	}
	if _, ok := wckRotationOwed(t, st); !ok {
		t.Fatal("remote device.lost apply did not arm the owed-rotation marker")
	}
}

// TestSnapshotImportFlipOwesRotation: importing a snapshot that carries a
// revocation (the P7-SYNC-01 bootstrap path) also arms the owed marker.
func TestSnapshotImportFlipOwesRotation(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	addRemoteDeviceForApplyTest(t, st, "device-b", "approved")
	seedKeyEpoch(t, st, 2)

	snap := Snapshot{Trust: []SnapshotTrust{{DeviceID: "device-b", State: "revoked"}}}
	if err := ImportSnapshot(ctx, st, snap, "sha-test", "hub-test"); err != nil {
		t.Fatal(err)
	}
	rec, ok := wckRotationOwed(t, st)
	if !ok {
		t.Fatal("snapshot import flip did not arm the owed-rotation marker")
	}
	if rec.Epoch != 2 {
		t.Fatalf("owed marker epoch=%d, want 2", rec.Epoch)
	}
}

// TestKeylessDeviceNeverOwesRotation: a device holding no key (epoch 0) flips
// trust but does NOT arm the marker — it holds nothing the revoked device could
// read, and its rotation gate skips epoch 0, so an armed marker would strand.
func TestKeylessDeviceNeverOwesRotation(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	signingA := addRemoteDeviceForApplyTest(t, st, "device-a", "approved")
	addRemoteDeviceForApplyTest(t, st, "device-b", "approved")
	now := time.Now().UnixMilli()

	revoke := signedDeviceTrustEvent(t, signingA, "evt_revoke_b", "device-a", 1, now, EventDeviceRevoked, "device-b")
	if _, err := ApplyEvents(ctx, st, []state.Event{revoke}); err != nil {
		t.Fatal(err)
	}
	if got := deviceTrustState(t, st, "device-b"); got != "revoked" {
		t.Fatalf("device-b trust=%q, want revoked (flip still happens keyless)", got)
	}
	if _, ok := wckRotationOwed(t, st); ok {
		t.Fatal("keyless device (epoch 0) armed the owed marker; want no marker")
	}
}

// TestOwedMarkerStormGuardPreservesSince: a replay (no flip) never touches the
// marker, and a LATER distinct revoke flip leaves the original "owed since"
// clock untouched — the storm-guard the sync warning and doctor rely on.
func TestOwedMarkerStormGuardPreservesSince(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	signingA := addRemoteDeviceForApplyTest(t, st, "device-a", "approved")
	addRemoteDeviceForApplyTest(t, st, "device-b", "approved")
	addRemoteDeviceForApplyTest(t, st, "device-c", "approved")
	seedKeyEpoch(t, st, 1)
	now := time.Now().UnixMilli()

	revokeB := signedDeviceTrustEvent(t, signingA, "evt_revoke_b", "device-a", 1, now, EventDeviceRevoked, "device-b")
	if _, err := ApplyEvents(ctx, st, []state.Event{revokeB}); err != nil {
		t.Fatal(err)
	}
	first, ok := wckRotationOwed(t, st)
	if !ok {
		t.Fatal("first revoke did not arm the marker")
	}

	// Replay the same revoke (changed=false): marker must be byte-identical.
	if _, err := ApplyEvents(ctx, st, []state.Event{revokeB}); err != nil {
		t.Fatal(err)
	}
	afterReplay, _ := wckRotationOwed(t, st)
	if !afterReplay.Since.Equal(first.Since) {
		t.Fatalf("replay changed owed Since %v -> %v, want unchanged", first.Since, afterReplay.Since)
	}

	// A later DISTINCT revoke flip (device-c) must preserve the original clock.
	revokeC := signedDeviceTrustEvent(t, signingA, "evt_revoke_c", "device-a", 2, now+1, EventDeviceRevoked, "device-c")
	if _, err := ApplyEvents(ctx, st, []state.Event{revokeC}); err != nil {
		t.Fatal(err)
	}
	if got := deviceTrustState(t, st, "device-c"); got != "revoked" {
		t.Fatalf("device-c trust=%q, want revoked", got)
	}
	afterSecond, _ := wckRotationOwed(t, st)
	if !afterSecond.Since.Equal(first.Since) {
		t.Fatalf("second flip reset owed Since %v -> %v, want the original preserved", first.Since, afterSecond.Since)
	}
}
