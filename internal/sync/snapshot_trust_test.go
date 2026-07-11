// snapshot_trust_test.go pins the P7-SYNC-01 fix: terminal device trust
// (revoked/lost) rides in the snapshot document, is re-derived on import
// exactly like replaying the trust event, and the snapshot version bump fails
// closed in both directions.
package sync

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/state"
)

// TestBuildSnapshotCarriesTerminalTrust: only revoked/lost rows ride in the
// snapshot, in deterministic id order; approved/pending/local are excluded.
func TestBuildSnapshotCarriesTerminalTrust(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	addRemoteDeviceForApplyTest(t, st, "device-a", "approved")
	addRemoteDeviceForApplyTest(t, st, "device-b", "revoked")
	addRemoteDeviceForApplyTest(t, st, "device-c", "lost")
	addRemoteDeviceForApplyTest(t, st, "device-d", "pending")

	snap, err := BuildSnapshot(ctx, st, "device-a", 1000, Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Trust) != 2 {
		t.Fatalf("trust rows = %+v, want exactly device-b revoked + device-c lost", snap.Trust)
	}
	if snap.Trust[0].DeviceID != "device-b" || snap.Trust[0].State != "revoked" {
		t.Fatalf("trust[0] = %+v, want device-b revoked", snap.Trust[0])
	}
	if snap.Trust[1].DeviceID != "device-c" || snap.Trust[1].State != "lost" {
		t.Fatalf("trust[1] = %+v, want device-c lost", snap.Trust[1])
	}
}

// TestImportSnapshotTrustFlipsAndFlagsRotation: importing a snapshot that
// carries a revocation flips approved→revoked and pending→lost, and flags
// encrypted bindings for rotation — the same behavior as replaying the
// device.revoked event (the compacted-away path this projection replaces).
func TestImportSnapshotTrustFlipsAndFlagsRotation(t *testing.T) {
	ctx := context.Background()
	st, device := newSyncStore(t)
	signingA := addRemoteDeviceForApplyTest(t, st, "device-a", "approved")
	addRemoteDeviceForApplyTest(t, st, "device-b", "approved")
	addRemoteDeviceForApplyTest(t, st, "device-c", "pending")
	now := time.Now().UnixMilli()

	// Seed a project + captured profile so there is a binding to flag.
	add := projEvent(t, device.ID, EventProjectAdded, now, "work/acme/api", "github.com/acme/api")
	env := signedEnvProfileEvent(t, signingA, "evt_env_seed", "device-a", 1, now+1, EnvProfilePayload{
		Path: "work/acme/api", Profile: "default", Provider: "devstrap_encrypted",
		Mode: "hydrate_or_runtime", BlobRef: "age_blob:seed", VarNames: []string{"API_TOKEN"},
	})
	if _, err := ApplyEvents(ctx, st, []state.Event{add, env}); err != nil {
		t.Fatal(err)
	}

	snap := Snapshot{Trust: []SnapshotTrust{
		{DeviceID: "device-b", State: "revoked"},
		{DeviceID: "device-c", State: "lost"},
	}}
	if err := ImportSnapshot(ctx, st, snap, "sha-test", "hub-test"); err != nil {
		t.Fatal(err)
	}
	if got := deviceTrustState(t, st, "device-b"); got != "revoked" {
		t.Fatalf("device-b trust=%q, want revoked", got)
	}
	if got := deviceTrustState(t, st, "device-c"); got != "lost" {
		t.Fatalf("device-c trust=%q, want lost", got)
	}
	n, err := st.CountSecretBindingsNeedingRotation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("needs_rotation count=%d, want 1", n)
	}

	// Re-import after the operator rotates and clears: sticky no-op, no re-flag.
	project, err := st.ProjectByPath(ctx, "work/acme/api")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClearRotationForProject(ctx, project.ID); err != nil {
		t.Fatal(err)
	}
	if err := ImportSnapshot(ctx, st, snap, "sha-test", "hub-test"); err != nil {
		t.Fatal(err)
	}
	if got := deviceTrustState(t, st, "device-b"); got != "revoked" {
		t.Fatalf("device-b trust=%q after re-import, want revoked", got)
	}
	n, err = st.CountSecretBindingsNeedingRotation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("needs_rotation count=%d after re-import, want 0 (replay must not re-flag)", n)
	}
}

// TestImportSnapshotTrustNeverFlipsLocalDevice: a hostile or defective
// snapshot naming the importer's own device must not talk it into
// distrusting itself — same posture as ApplyRemoteDeviceTrustTx.
func TestImportSnapshotTrustNeverFlipsLocalDevice(t *testing.T) {
	ctx := context.Background()
	st, device := newSyncStore(t)

	snap := Snapshot{Trust: []SnapshotTrust{{DeviceID: device.ID, State: "revoked"}}}
	if err := ImportSnapshot(ctx, st, snap, "sha-test", "hub-test"); err != nil {
		t.Fatal(err)
	}
	if got := deviceTrustState(t, st, device.ID); got != "local" {
		t.Fatalf("local device trust=%q, want local (never flips from a snapshot)", got)
	}
}

// TestImportSnapshotTrustUnknownTargetCreatesRevokedPlaceholder: device records
// do not sync, so the target may be unknown to the importer; a placeholder row
// is created and immediately flipped (pending→revoked is fail-closed).
func TestImportSnapshotTrustUnknownTargetCreatesRevokedPlaceholder(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)

	snap := Snapshot{Trust: []SnapshotTrust{{DeviceID: "device-unseen", State: "revoked"}}}
	if err := ImportSnapshot(ctx, st, snap, "sha-test", "hub-test"); err != nil {
		t.Fatal(err)
	}
	if got := deviceTrustState(t, st, "device-unseen"); got != "revoked" {
		t.Fatalf("unknown target trust=%q, want revoked placeholder", got)
	}
}

// TestImportSnapshotTrustMalformedAbortsWholeImport: a malformed trust row is
// a hard error and nothing else from the snapshot lands (single-tx import).
func TestImportSnapshotTrustMalformedAbortsWholeImport(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)

	for _, bad := range []SnapshotTrust{
		{DeviceID: "device-x", State: "approved"}, // trust can only descend via snapshot
		{DeviceID: "device-x", State: "banana"},
		{DeviceID: "", State: "revoked"},
	} {
		snap := Snapshot{
			Entries: []SnapshotEntry{gitEntry("work/acme/api", "github.com/acme/api", h(100), "dev_a", "evt_1")},
			Trust:   []SnapshotTrust{bad},
		}
		err := ImportSnapshot(ctx, st, snap, "sha-test", "hub-test")
		if err == nil {
			t.Fatalf("trust row %+v: import succeeded, want hard error", bad)
		}
		if _, perr := st.ProjectByPath(ctx, "work/acme/api"); perr == nil {
			t.Fatalf("trust row %+v: namespace entry landed despite the aborted import (tx not atomic)", bad)
		}
	}
}

// TestImportSnapshotTrustEqualsReplay: importing the trust projection is
// exactly equivalent to replaying the signed device.revoked event, and
// import-after-replay is a no-op — the property that makes State-only rows
// (no source-event coordinates) sufficient.
func TestImportSnapshotTrustEqualsReplay(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// Store 1 replays the signed revoke event.
	st1, _ := newSyncStore(t)
	signingA := addRemoteDeviceForApplyTest(t, st1, "device-a", "approved")
	addRemoteDeviceForApplyTest(t, st1, "device-b", "approved")
	revoke := signedDeviceTrustEvent(t, signingA, "evt_revoke_b", "device-a", 1, now, EventDeviceRevoked, "device-b")
	if _, err := ApplyEvents(ctx, st1, []state.Event{revoke}); err != nil {
		t.Fatal(err)
	}

	// Store 2 imports the equivalent snapshot projection.
	st2, _ := newSyncStore(t)
	addRemoteDeviceForApplyTest(t, st2, "device-a", "approved")
	addRemoteDeviceForApplyTest(t, st2, "device-b", "approved")
	snap := Snapshot{Trust: []SnapshotTrust{{DeviceID: "device-b", State: "revoked"}}}
	if err := ImportSnapshot(ctx, st2, snap, "sha-test", "hub-test"); err != nil {
		t.Fatal(err)
	}

	if got1, got2 := deviceTrustState(t, st1, "device-b"), deviceTrustState(t, st2, "device-b"); got1 != got2 || got1 != "revoked" {
		t.Fatalf("replay=%q import=%q, want both revoked (import ≡ replay)", got1, got2)
	}

	// Import after replay is a sticky no-op.
	if err := ImportSnapshot(ctx, st1, snap, "sha-test", "hub-test"); err != nil {
		t.Fatal(err)
	}
	if got := deviceTrustState(t, st1, "device-b"); got != "revoked" {
		t.Fatalf("device-b trust=%q after import-then-replay, want revoked", got)
	}
}

// TestSnapshotVersionFailClosed pins the v2 bump (P7-SYNC-01): a v1
// envelope/document is refused with ErrSnapshotVerification (a v1 snapshot
// silently lacks the trust projection), SealSnapshot stamps v2, retention
// manifests stay readable at v1 (floors are trust-neutral) but are written at
// v2, and unknown future versions are refused everywhere.
func TestSnapshotVersionFailClosed(t *testing.T) {
	wck, err := NewWCK()
	if err != nil {
		t.Fatal(err)
	}

	// SealSnapshot stamps the current version.
	obj, _, err := SealSnapshot(testSnapshot(), wck, 3)
	if err != nil {
		t.Fatal(err)
	}
	if snap, err := UnsealSnapshot(obj, wck); err != nil || snap.V != 2 {
		t.Fatalf("sealed snapshot V=%d err=%v, want V=2", snap.V, err)
	}

	// A v1 envelope is refused before any decryption.
	v1env := snapshotEnvelope{V: 1, WorkspaceID: "ws_test", ProducedBy: "dev_a", HLC: 1000, Epoch: 3, KID: KIDForWCK(wck), CT: base64.StdEncoding.EncodeToString([]byte("junk"))}
	rawEnv, err := json.Marshal(v1env)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseSnapshotEnvelope(rawEnv); !errors.Is(err, ErrSnapshotVerification) {
		t.Fatalf("v1 envelope parse: got %v, want ErrSnapshotVerification", err)
	}
	if _, err := UnsealSnapshot(rawEnv, wck); !errors.Is(err, ErrSnapshotVerification) {
		t.Fatalf("v1 envelope unseal: got %v, want ErrSnapshotVerification", err)
	}

	// A v1 DOCUMENT inside a well-formed v2 envelope is refused after the open:
	// hand-seal a v1 plaintext under the v2 carrier.
	snap := testSnapshot()
	snap.V = 1
	snap.Epoch = 3
	snap.KID = KIDForWCK(wck)
	plaintext, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := aeadFor(wck)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, aead.NonceSize())
	aad := snapshotAAD(snap.WorkspaceID, snap.ProducedBy, KIDForWCK(wck), snap.HLC, snap.Epoch)
	ct := aead.Seal(nil, nonce, plaintext, aad)
	env := snapshotEnvelope{V: 2, WorkspaceID: snap.WorkspaceID, ProducedBy: snap.ProducedBy, HLC: snap.HLC, Epoch: snap.Epoch, KID: snap.KID, CT: base64.StdEncoding.EncodeToString(append(nonce, ct...))}
	rawEnv, err = json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnsealSnapshot(rawEnv, wck); !errors.Is(err, ErrSnapshotVerification) || !strings.Contains(err.Error(), "document version 1") {
		t.Fatalf("v1 document unseal: got %v, want ErrSnapshotVerification on document version 1", err)
	}
}

// TestRetentionManifestVersionCompat: v1 manifests (published before this
// binary) parse and verify — the floors map is trust-neutral, and the first
// upgraded compactor must reconcile the pre-existing v1 manifest before it can
// publish v2 — while SignRetentionManifest stamps v2 and unknown future
// versions fail closed in both readers.
func TestRetentionManifestVersionCompat(t *testing.T) {
	private, public := testSigningKeys(t)

	mk := func(v int) RetentionManifest {
		return RetentionManifest{
			V:           v,
			WorkspaceID: "ws_test",
			Floors:      map[string]int64{"dev_a": 5},
			Snapshot:    RetentionSnapshotRef{Epoch: 3, HLC: 1000, KID: "kid", ProducedBy: "dev_a", SHA256: "abc"},
			ProducedBy:  "dev_a",
			ProducedAt:  1000,
		}
	}

	// SignRetentionManifest stamps the current version.
	signed := mk(0)
	if err := SignRetentionManifest(&signed, private); err != nil {
		t.Fatal(err)
	}
	if signed.V != 2 {
		t.Fatalf("signed manifest V=%d, want 2", signed.V)
	}
	if err := VerifyRetentionManifest(signed, public); err != nil {
		t.Fatal(err)
	}

	// A v1 manifest (hand-signed, as an old binary would have) still parses and
	// verifies.
	old := mk(1)
	sig, err := devicekeys.Sign(private, RetentionSignatureDomain, RetentionSignaturePayload(old))
	if err != nil {
		t.Fatal(err)
	}
	old.Sig = sig
	if err := VerifyRetentionManifest(old, public); err != nil {
		t.Fatalf("v1 manifest verify: %v, want accepted (floors are trust-neutral)", err)
	}
	rawOld, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseRetentionManifest(rawOld); err != nil {
		t.Fatalf("v1 manifest parse: %v, want accepted", err)
	}

	// An unknown future version fails closed in both readers.
	future := mk(3)
	sig, err = devicekeys.Sign(private, RetentionSignatureDomain, RetentionSignaturePayload(future))
	if err != nil {
		t.Fatal(err)
	}
	future.Sig = sig
	if err := VerifyRetentionManifest(future, public); err == nil {
		t.Fatal("v3 manifest verify succeeded, want fail-closed refusal")
	}
	rawFuture, err := json.Marshal(future)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseRetentionManifest(rawFuture); err == nil {
		t.Fatal("v3 manifest parse succeeded, want fail-closed refusal")
	}
}
