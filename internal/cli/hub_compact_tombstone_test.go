package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
)

// writeSignedAck publishes a signed ack marker for deviceID to the file hub.
func writeSignedAck(t *testing.T, env *recoveryEnv, deviceID, signingPriv string, watermark int64) {
	t.Helper()
	m := dssync.AckMarker{
		DeviceID:     deviceID,
		HLCWatermark: watermark,
		ProducedAt:   watermark,
		WorkspaceID:  env.wsID,
	}
	if err := dssync.SignAckMarker(&m, signingPriv); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := (dssync.FileHub{Path: env.hubPath}).PutAck(env.ctx, deviceID, raw); err != nil {
		t.Fatal(err)
	}
}

// tombstone marks an existing project deleted at the given HLC.
func tombstone(t *testing.T, env *recoveryEnv, store *state.Store, path string, hlc int64) {
	t.Helper()
	if err := store.WithTx(env.ctx, func(tx *state.Tx) error {
		return tx.TombstoneProject(env.ctx, path, hlc)
	}); err != nil {
		t.Fatal(err)
	}
}

// producedSnapshot fetches, unseals, and returns the snapshot the last compaction
// published (unsealed under B's epoch-1 WCK).
func producedSnapshot(t *testing.T, env *recoveryEnv, store *state.Store) dssync.Snapshot {
	t.Helper()
	fh := dssync.FileHub{Path: env.hubPath}
	raw, _, err := fh.GetRetention(env.ctx)
	if err != nil {
		t.Fatalf("get retention: %v", err)
	}
	m, err := dssync.ParseRetentionManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := fh.GetSnapshotObject(env.ctx, m.Snapshot.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	kr := buildKeyring(env.ctx, env.opts, store)
	if err := kr.Prime(env.ctx); err != nil {
		t.Fatal(err)
	}
	cands := kr.WCKCandidates(m.Snapshot.Epoch, m.Snapshot.KID)
	if len(cands) == 0 {
		t.Fatalf("no held WCK for epoch %d", m.Snapshot.Epoch)
	}
	snap, err := dssync.UnsealSnapshot(obj, cands[0])
	if err != nil {
		t.Fatalf("unseal produced snapshot: %v", err)
	}
	return snap
}

// TestHubCompactGCsAckedTombstone: with a verified ack from the only approved
// peer whose watermark is above the tombstone HLC, compaction purges the
// tombstone and the produced snapshot omits it.
func TestHubCompactGCsAckedTombstone(t *testing.T) {
	env, store, _ := setupCompact(t)
	defer closeStore(store)
	tombstone(t, env, store, "work/web", 100)
	writeSignedAck(t, env, recoveryProducer, env.prodSign.Private, 200)

	if err := hubCompact(env.ctx, io.Discard, io.Discard, env.opts, store, env.hub(t, store), env.hubID, env.paths, 2, 0, true, false); err != nil {
		t.Fatalf("hubCompact: %v", err)
	}
	if n, err := store.CountTombstonesBelowHLC(env.ctx, 1<<62); err != nil || n != 0 {
		t.Fatalf("tombstone not GC'd: count=%d err=%v", n, err)
	}
	snap := producedSnapshot(t, env, store)
	for _, ts := range snap.Tombstones {
		if ts.PathKey == "work/web" {
			t.Fatalf("GC'd tombstone still present in produced snapshot: %+v", ts)
		}
	}
}

// TestHubCompactSkipsGCWithoutPeerAck: a missing approved-device ack retains the
// tombstone and prints a naming hint.
func TestHubCompactSkipsGCWithoutPeerAck(t *testing.T) {
	env, store, _ := setupCompact(t)
	defer closeStore(store)
	tombstone(t, env, store, "work/web", 100)
	// No ack written for recoveryProducer.

	var errOut bytes.Buffer
	if err := hubCompact(env.ctx, io.Discard, &errOut, env.opts, store, env.hub(t, store), env.hubID, env.paths, 2, 0, true, false); err != nil {
		t.Fatalf("hubCompact: %v", err)
	}
	if n, err := store.CountTombstonesBelowHLC(env.ctx, 1<<62); err != nil || n != 1 {
		t.Fatalf("tombstone must be retained: count=%d err=%v", n, err)
	}
	if !strings.Contains(errOut.String(), "has never written a verified sync ack") {
		t.Fatalf("missing GC-skip hint; stderr = %q", errOut.String())
	}
}

// TestHubCompactGCIgnoresRevokedAndUnverifiedAcks: a revoked device's low
// watermark must not pull the GC floor down, and an unverified ack must not
// count as the required peer ack.
func TestHubCompactGCIgnoresRevokedAck(t *testing.T) {
	env, store, _ := setupCompact(t)
	defer closeStore(store)
	tombstone(t, env, store, "work/web", 100)
	// Approved peer acks well above the tombstone.
	writeSignedAck(t, env, recoveryProducer, env.prodSign.Private, 200)
	// A revoked device acks BELOW the tombstone; it must be ignored.
	revSign, err := devicekeys.NewSigningIdentity()
	if err != nil {
		t.Fatal(err)
	}
	revAge, err := devicekeys.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertDevice(env.ctx, state.Device{
		ID: "dev_revoked", Name: "rev", OS: "linux", Arch: "arm64",
		PublicKey: revAge.Recipient, SigningPublicKey: revSign.Public, TrustState: "revoked",
	}); err != nil {
		t.Fatal(err)
	}
	writeSignedAck(t, env, "dev_revoked", revSign.Private, 50)

	if err := hubCompact(env.ctx, io.Discard, io.Discard, env.opts, store, env.hub(t, store), env.hubID, env.paths, 2, 0, true, false); err != nil {
		t.Fatalf("hubCompact: %v", err)
	}
	// beforeHLC = min(localWM, 200) = 200 (revoked's 50 ignored); 100 < 200 → GC'd.
	if n, err := store.CountTombstonesBelowHLC(env.ctx, 1<<62); err != nil || n != 0 {
		t.Fatalf("revoked ack was not ignored (tombstone survived): count=%d err=%v", n, err)
	}
}

// TestHubCompactGCSkipsOnUnverifiedAck: a tampered (signature-broken) ack from
// the approved peer is ignored, so GC is skipped as if the ack were missing.
func TestHubCompactGCSkipsOnUnverifiedAck(t *testing.T) {
	env, store, _ := setupCompact(t)
	defer closeStore(store)
	tombstone(t, env, store, "work/web", 100)
	// Sign a valid ack, then tamper the watermark so the signature no longer
	// covers the bytes.
	m := dssync.AckMarker{DeviceID: recoveryProducer, HLCWatermark: 200, ProducedAt: 200, WorkspaceID: env.wsID}
	if err := dssync.SignAckMarker(&m, env.prodSign.Private); err != nil {
		t.Fatal(err)
	}
	m.HLCWatermark = 999999 // tamper after signing
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := (dssync.FileHub{Path: env.hubPath}).PutAck(env.ctx, recoveryProducer, raw); err != nil {
		t.Fatal(err)
	}

	if err := hubCompact(env.ctx, io.Discard, io.Discard, env.opts, store, env.hub(t, store), env.hubID, env.paths, 2, 0, true, false); err != nil {
		t.Fatalf("hubCompact: %v", err)
	}
	if n, err := store.CountTombstonesBelowHLC(env.ctx, 1<<62); err != nil || n != 1 {
		t.Fatalf("unverified ack must not authorize GC (tombstone retained): count=%d err=%v", n, err)
	}
}

// TestHubCompactTombstoneResurrectionRegression: after a tombstone is GC'd, a
// legitimately newer add re-creates the path (a restore, not a resurrection).
func TestHubCompactTombstoneResurrectionRegression(t *testing.T) {
	env, store, _ := setupCompact(t)
	defer closeStore(store)
	tombstone(t, env, store, "work/web", 100)
	writeSignedAck(t, env, recoveryProducer, env.prodSign.Private, 200)

	if err := hubCompact(env.ctx, io.Discard, io.Discard, env.opts, store, env.hub(t, store), env.hubID, env.paths, 2, 0, true, false); err != nil {
		t.Fatalf("hubCompact: %v", err)
	}
	if n, err := store.CountTombstonesBelowHLC(env.ctx, 1<<62); err != nil || n != 0 {
		t.Fatalf("tombstone not GC'd: count=%d err=%v", n, err)
	}
	// A legitimately newer add (minted now, HLC far above the old delete)
	// re-creates the path — a restore, not a resurrection.
	if _, err := addProject(env.ctx, store, env.opts, "git@github.com:acme/web.git", "work/web", "", ""); err != nil {
		t.Fatalf("restore add: %v", err)
	}
	p, err := store.ProjectByPath(env.ctx, "work/web")
	if err != nil {
		t.Fatalf("restored project missing: %v", err)
	}
	if p.Status != "active" {
		t.Fatalf("restored project status = %q, want active", p.Status)
	}
}
