package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/config"
	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
	"github.com/spf13/viper"
)

const recoveryProducer = "dev_producer_a"

type recoveryEnv struct {
	ctx      context.Context
	opts     *options
	paths    config.Paths
	hubPath  string
	hubID    string
	wsID     string
	prodSign devicekeys.SigningIdentity
}

// setupRecovery builds an importing device B with a pinned, approved producer A.
// When bootstrapWCK is true, B mints (and holds) the epoch-1 WCK and the returned
// wck is B's own key; otherwise the returned wck is a standalone key B does NOT
// hold (the keyless-joiner case).
func setupRecovery(t *testing.T, bootstrapWCK bool) (*recoveryEnv, *state.Store, []byte) {
	t.Helper()
	t.Setenv(platform.NoKeychainEnv, "1")
	ctx := context.Background()
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	opts := &options{v: viper.New()}
	opts.v.Set("home", home)
	opts.v.Set("root", root)
	paths := opts.paths()
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(ctx, paths.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureWorkspace(ctx, "b", root); err != nil {
		t.Fatal(err)
	}
	local, err := store.EnsureDevice(ctx, "b")
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureLocalDeviceIdentity(ctx, paths, store, local); err != nil {
		t.Fatal(err)
	}
	wsID, err := store.WorkspaceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	prodAge, err := devicekeys.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	prodSign, err := devicekeys.NewSigningIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertDevice(ctx, state.Device{
		ID: recoveryProducer, Name: "producer a", OS: "linux", Arch: "arm64",
		PublicKey: prodAge.Recipient, SigningPublicKey: prodSign.Public, TrustState: "approved",
	}); err != nil {
		t.Fatal(err)
	}

	var wck []byte
	if bootstrapWCK {
		kr := buildKeyring(ctx, opts, store)
		if _, err := kr.EnsureBootstrap(ctx); err != nil {
			t.Fatal(err)
		}
		if err := kr.Prime(ctx); err != nil {
			t.Fatal(err)
		}
		cands := kr.WCKCandidates(1, "")
		if len(cands) == 0 {
			t.Fatal("bootstrapped WCK not held")
		}
		wck = cands[0]
	} else {
		wck, err = dssync.NewWCK()
		if err != nil {
			t.Fatal(err)
		}
	}
	hubPath := filepath.Join(t.TempDir(), "hub.json")
	if err := os.WriteFile(hubPath, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &recoveryEnv{
		ctx: ctx, opts: opts, paths: paths, hubPath: hubPath,
		hubID: "file:" + hubPath, wsID: wsID, prodSign: prodSign,
	}, store, wck
}

func recoverySnapshot(wsID string) dssync.Snapshot {
	return dssync.Snapshot{
		WorkspaceID: wsID,
		ProducedBy:  recoveryProducer,
		HLC:         time.Now().UnixMilli() << 16,
		Floor:       dssync.Cursor{recoveryProducer: 5},
		Anchors:     []dssync.ChainAnchor{{DeviceID: recoveryProducer, Seq: 4, ContentHash: "sha256:anchor", HLC: 100}},
		Entries: []dssync.SnapshotEntry{{
			Path: "work/api", PathKey: "work/api", Type: "git_repo", Status: "active",
			SourceEventHLC: 700, SourceEventDeviceID: recoveryProducer, SourceEventID: "evt_api",
			Git: &dssync.SnapshotGit{RemoteURL: "git@github.com:acme/api.git", RemoteKey: "github.com/acme/api", DefaultBranch: "main"},
		}},
	}
}

// publishSnapshot seals the snapshot under wck and writes the object plus a
// manifest signed by signingPriv to the FileHub, returning the object sha256.
func publishSnapshot(t *testing.T, env *recoveryEnv, snap dssync.Snapshot, wck []byte, epoch int64, signingPriv string) string {
	t.Helper()
	obj, sha, err := dssync.SealSnapshot(snap, wck, epoch)
	if err != nil {
		t.Fatal(err)
	}
	fh := dssync.FileHub{Path: env.hubPath}
	if err := fh.PutSnapshotObject(env.ctx, sha, obj); err != nil {
		t.Fatal(err)
	}
	m := dssync.RetentionManifest{
		WorkspaceID: snap.WorkspaceID,
		Floors:      map[string]int64(snap.Floor),
		Snapshot: dssync.RetentionSnapshotRef{
			Epoch: epoch, HLC: snap.HLC, KID: dssync.KIDForWCK(wck), ProducedBy: snap.ProducedBy, SHA256: sha,
		},
		ProducedBy: snap.ProducedBy,
		ProducedAt: snap.HLC,
	}
	if err := dssync.SignRetentionManifest(&m, signingPriv); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := fh.PutRetention(env.ctx, raw, ""); err != nil {
		t.Fatal(err)
	}
	return sha
}

func (env *recoveryEnv) hub(t *testing.T, store *state.Store) dssync.Hub {
	t.Helper()
	hub, _, err := hubFromOptions(env.ctx, env.opts, store, env.hubPath)
	if err != nil {
		t.Fatal(err)
	}
	return hub
}

// TestRecoverFromSnapshotBootstrapsFreshDevice: a device whose cursor is below
// the hub floor recovers by importing the snapshot; the project lands and the
// per-device cursor advances to floor-1 so the next pull is incremental.
func TestRecoverFromSnapshotBootstrapsFreshDevice(t *testing.T) {
	env, store, wck := setupRecovery(t, true)
	defer closeStore(store)
	snap := recoverySnapshot(env.wsID)
	publishSnapshot(t, env, snap, wck, 1, env.prodSign.Private)

	// A fresh pull must demand a snapshot.
	if _, err := pullAndApplyEvents(env.ctx, store, env.hub(t, store), env.hubID); !errors.Is(err, dssync.ErrSnapshotRequired) {
		t.Fatalf("pre-recovery pull err = %v, want ErrSnapshotRequired", err)
	}

	var out bytes.Buffer
	imported, err := recoverFromSnapshot(env.ctx, &out, store, env.hub(t, store), env.hubID, env.paths, buildKeyring(env.ctx, env.opts, store))
	if err != nil {
		t.Fatalf("recoverFromSnapshot: %v", err)
	}
	if !imported {
		t.Fatal("imported = false, want true")
	}
	if _, err := store.ProjectByPath(env.ctx, "work/api"); err != nil {
		t.Fatalf("imported project missing: %v", err)
	}
	cursors, err := store.HubDeviceCursors(env.ctx, env.hubID)
	if err != nil {
		t.Fatal(err)
	}
	if cursors[recoveryProducer] != 4 {
		t.Fatalf("cursor for producer = %d, want 4 (floor-1)", cursors[recoveryProducer])
	}
	// The retry pull now succeeds incrementally.
	if _, err := pullAndApplyEvents(env.ctx, store, env.hub(t, store), env.hubID); err != nil {
		t.Fatalf("post-recovery incremental pull: %v", err)
	}
}

// TestRecoverFromSnapshotRefusesUnpinnedProducer: a manifest whose producer is
// not a locally approved device is refused (ErrSnapshotVerification), and no
// state or cursor changes.
func TestRecoverFromSnapshotRefusesUnpinnedProducer(t *testing.T) {
	env, store, wck := setupRecovery(t, true)
	defer closeStore(store)
	// Demote the producer so it is no longer approved.
	if err := store.SetDeviceTrustState(env.ctx, recoveryProducer, "pending"); err != nil {
		t.Fatal(err)
	}
	snap := recoverySnapshot(env.wsID)
	publishSnapshot(t, env, snap, wck, 1, env.prodSign.Private)

	var out bytes.Buffer
	imported, err := recoverFromSnapshot(env.ctx, &out, store, env.hub(t, store), env.hubID, env.paths, buildKeyring(env.ctx, env.opts, store))
	if imported || !errors.Is(err, dssync.ErrSnapshotVerification) {
		t.Fatalf("imported=%v err=%v, want refusal with ErrSnapshotVerification", imported, err)
	}
	var appErr appError
	if !errors.As(err, &appErr) || appErr.code != exitInvalidConfig {
		t.Fatalf("exit code = %v, want exitInvalidConfig", err)
	}
	if _, perr := store.ProjectByPath(env.ctx, "work/api"); perr == nil {
		t.Fatal("state changed despite refusal")
	}
	if cursors, _ := store.HubDeviceCursors(env.ctx, env.hubID); cursors[recoveryProducer] != 0 {
		t.Fatalf("cursor advanced despite refusal: %d", cursors[recoveryProducer])
	}
}

// TestRecoverFromSnapshotKeylessJoinerDefers: a device that holds no WCK for the
// snapshot epoch defers (imported=false, nil error) and imports nothing.
func TestRecoverFromSnapshotKeylessJoinerDefers(t *testing.T) {
	env, store, wck := setupRecovery(t, false) // B holds no WCK
	defer closeStore(store)
	snap := recoverySnapshot(env.wsID)
	publishSnapshot(t, env, snap, wck, 1, env.prodSign.Private)

	var out bytes.Buffer
	imported, err := recoverFromSnapshot(env.ctx, &out, store, env.hub(t, store), env.hubID, env.paths, buildKeyring(env.ctx, env.opts, store))
	if err != nil {
		t.Fatalf("keyless recovery should defer with nil error, got %v", err)
	}
	if imported {
		t.Fatal("imported = true, want false (keyless defer)")
	}
	if _, perr := store.ProjectByPath(env.ctx, "work/api"); perr == nil {
		t.Fatal("keyless joiner imported state without a key")
	}
}

// TestRecoverFromSnapshotFloorRollbackWarns: when the hub serves a floor below a
// previously-cached one, recovery warns loudly and still imports.
func TestRecoverFromSnapshotFloorRollbackWarns(t *testing.T) {
	env, store, wck := setupRecovery(t, true)
	defer closeStore(store)
	// Cache a higher verified floor than the manifest will serve.
	cache, _ := json.Marshal(map[string]int64{recoveryProducer: 9})
	if err := store.SetLocalMeta(env.ctx, dssync.RetentionFloorMetaKey(env.hubID), string(cache)); err != nil {
		t.Fatal(err)
	}
	snap := recoverySnapshot(env.wsID) // floor 5 < cached 9
	publishSnapshot(t, env, snap, wck, 1, env.prodSign.Private)

	var out bytes.Buffer
	imported, err := recoverFromSnapshot(env.ctx, &out, store, env.hub(t, store), env.hubID, env.paths, buildKeyring(env.ctx, env.opts, store))
	if err != nil {
		t.Fatalf("recoverFromSnapshot: %v", err)
	}
	if !imported {
		t.Fatal("imported = false, want true")
	}
	if !bytes.Contains(out.Bytes(), []byte("rolled back")) {
		t.Fatalf("expected a rollback warning, got: %s", out.String())
	}
}

// TestRecoverFromSnapshotShaMismatchRefused: a snapshot object whose bytes do not
// hash to the manifest's sha256 is refused before import.
func TestRecoverFromSnapshotShaMismatchRefused(t *testing.T) {
	env, store, wck := setupRecovery(t, true)
	defer closeStore(store)
	snap := recoverySnapshot(env.wsID)
	sha := publishSnapshot(t, env, snap, wck, 1, env.prodSign.Private)
	// Overwrite the snapshot object with different bytes under the same key so
	// its sha no longer matches the (signed) manifest reference.
	fh := dssync.FileHub{Path: env.hubPath}
	snapDir := filepath.Join(filepath.Dir(env.hubPath), "hub-snapshots")
	if err := os.WriteFile(filepath.Join(snapDir, sha+".json"), []byte(`{"v":1,"tampered":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = fh

	var out bytes.Buffer
	imported, err := recoverFromSnapshot(env.ctx, &out, store, env.hub(t, store), env.hubID, env.paths, buildKeyring(env.ctx, env.opts, store))
	if imported || !errors.Is(err, dssync.ErrSnapshotVerification) {
		t.Fatalf("imported=%v err=%v, want ErrSnapshotVerification on sha mismatch", imported, err)
	}
}

// TestRecoverFromSnapshotRefusesForeignProducerSnapshot pins the post-review
// producer-identity check: a manifest legitimately SIGNED by the approved
// producer but naming a DIFFERENT device as the snapshot's producer must be
// refused — device B's payload can never ride device A's signature.
func TestRecoverFromSnapshotRefusesForeignProducerSnapshot(t *testing.T) {
	env, store, wck := setupRecovery(t, true)
	defer closeStore(store)
	snap := recoverySnapshot(env.wsID)
	obj, sha, err := dssync.SealSnapshot(snap, wck, 1)
	if err != nil {
		t.Fatal(err)
	}
	fh := dssync.FileHub{Path: env.hubPath}
	if err := fh.PutSnapshotObject(env.ctx, sha, obj); err != nil {
		t.Fatal(err)
	}
	m := dssync.RetentionManifest{
		WorkspaceID: snap.WorkspaceID,
		Floors:      map[string]int64(snap.Floor),
		Snapshot: dssync.RetentionSnapshotRef{
			Epoch: 1, HLC: snap.HLC, KID: dssync.KIDForWCK(wck),
			ProducedBy: "dev_other_b", // NOT the manifest signer
			SHA256:     sha,
		},
		ProducedBy: recoveryProducer, // signed by the approved producer
		ProducedAt: snap.HLC,
	}
	if err := dssync.SignRetentionManifest(&m, env.prodSign.Private); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := fh.PutRetention(env.ctx, raw, ""); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	imported, err := recoverFromSnapshot(env.ctx, &out, store, env.hub(t, store), env.hubID, env.paths, buildKeyring(env.ctx, env.opts, store))
	if imported || !errors.Is(err, dssync.ErrSnapshotVerification) {
		t.Fatalf("imported=%v err=%v, want ErrSnapshotVerification refusal", imported, err)
	}
	var appErr appError
	if !errors.As(err, &appErr) || appErr.code != exitInvalidConfig {
		t.Fatalf("exit code = %v, want exitInvalidConfig", err)
	}
	if _, perr := store.ProjectByPath(env.ctx, "work/api"); perr == nil {
		t.Fatal("state changed despite refusal")
	}
}
