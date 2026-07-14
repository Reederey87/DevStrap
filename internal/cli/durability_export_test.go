package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/config"
	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
	"github.com/spf13/viper"
)

func TestDurabilityExportSkipsPrimaryWithoutCompactionSnapshot(t *testing.T) {
	env, store, _ := setupRecovery(t, true)
	defer closeStore(store)
	replicaPath := filepath.Join(t.TempDir(), "replica.json")
	env.opts.v.Set("hub_replica", "file:"+replicaPath)
	env.opts.v.Set(durabilityExportConfigKey, "24h")

	var out bytes.Buffer
	if err := maybeExportHubDurability(env.ctx, &out, env.opts, store, env.hub(t, store), env.hubPath, time.Now()); err != nil {
		t.Fatalf("maybeExportHubDurability: %v", err)
	}
	if !strings.Contains(out.String(), "no compaction snapshot") {
		t.Fatalf("output = %q, want clear no-snapshot skip", out.String())
	}
	if _, ok, err := store.GetLocalMeta(env.ctx, durabilityExportMetaKey); err != nil || ok {
		t.Fatalf("last-success marker = ok %v err %v, want absent after skip", ok, err)
	}
}

func TestDurabilityExportHonorsSuccessfulExportInterval(t *testing.T) {
	env, store, _ := setupCompact(t)
	defer closeStore(store)
	if err := hubCompact(env.ctx, io.Discard, io.Discard, env.opts, store, env.hub(t, store), env.hubID, env.paths, 2, 0, true, false); err != nil {
		t.Fatalf("hubCompact: %v", err)
	}
	replicaPath := filepath.Join(t.TempDir(), "replica.json")
	env.opts.v.Set("hub_replica", "file:"+replicaPath)
	env.opts.v.Set(durabilityExportConfigKey, "24h")
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	if err := maybeExportHubDurability(env.ctx, io.Discard, env.opts, store, env.hub(t, store), env.hubPath, now); err != nil {
		t.Fatalf("first export: %v", err)
	}
	replica := dssync.FileHub{Path: replicaPath}
	raw, _, err := replica.GetRetention(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := dssync.ParseRetentionManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := replica.DeleteSnapshotObject(env.ctx, manifest.Snapshot.SHA256); err != nil {
		t.Fatal(err)
	}
	if err := maybeExportHubDurability(env.ctx, io.Discard, env.opts, store, env.hub(t, store), env.hubPath, now.Add(23*time.Hour)); err != nil {
		t.Fatalf("not-due export: %v", err)
	}
	if _, err := replica.GetSnapshotObject(env.ctx, manifest.Snapshot.SHA256); !errors.Is(err, dssync.ErrBlobNotFound) {
		t.Fatalf("not-due export rewrote deleted snapshot: %v", err)
	}
	if err := maybeExportHubDurability(env.ctx, io.Discard, env.opts, store, env.hub(t, store), env.hubPath, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("due export: %v", err)
	}
	if _, err := replica.GetSnapshotObject(env.ctx, manifest.Snapshot.SHA256); err != nil {
		t.Fatalf("due export did not restore snapshot: %v", err)
	}
}

func TestRunSyncCycleExportsDueSnapshot(t *testing.T) {
	env, store, _ := setupCompact(t)
	if err := hubCompact(env.ctx, io.Discard, io.Discard, env.opts, store, env.hub(t, store), env.hubID, env.paths, 2, 0, true, false); err != nil {
		closeStore(store)
		t.Fatalf("hubCompact: %v", err)
	}
	closeStore(store)
	replicaPath := filepath.Join(t.TempDir(), "replica.json")
	env.opts.v.Set("hub_replica", "file:"+replicaPath)
	env.opts.v.Set(durabilityExportConfigKey, "24h")
	if err := runSyncCycle(env.ctx, io.Discard, env.opts, env.hubPath, true, false); err != nil {
		t.Fatalf("runSyncCycle: %v", err)
	}
	replica := dssync.FileHub{Path: replicaPath}
	raw, _, err := replica.GetRetention(env.ctx)
	if err != nil {
		t.Fatalf("sync did not mirror retention: %v", err)
	}
	manifest, err := dssync.ParseRetentionManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replica.GetSnapshotObject(env.ctx, manifest.Snapshot.SHA256); err != nil {
		t.Fatalf("sync did not mirror snapshot: %v", err)
	}
}

func TestDurabilityReplicaOutageDoesNotFailPrimarySync(t *testing.T) {
	env, store, _ := setupCompact(t)
	if err := hubCompact(env.ctx, io.Discard, io.Discard, env.opts, store, env.hub(t, store), env.hubID, env.paths, 2, 0, true, false); err != nil {
		closeStore(store)
		t.Fatalf("hubCompact: %v", err)
	}
	closeStore(store)
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	env.opts.v.Set("hub_replica", "file:"+filepath.Join(blocker, "replica.json"))
	env.opts.v.Set(durabilityExportConfigKey, "24h")
	var out bytes.Buffer
	if err := runSyncCycle(env.ctx, &out, env.opts, env.hubPath, true, false); err != nil {
		t.Fatalf("runSyncCycle with unavailable replica: %v", err)
	}
	if !strings.Contains(out.String(), "warning: hub durability export failed after primary sync") {
		t.Fatalf("sync output = %q, want non-fatal durability warning", out.String())
	}
}

func TestDurabilityInvalidConfigurationStillFailsSync(t *testing.T) {
	env, store, _ := setupCompact(t)
	closeStore(store)
	env.opts.v.Set("hub_replica", "file:"+env.hubPath)
	err := runSyncCycle(env.ctx, io.Discard, env.opts, env.hubPath, true, false)
	var appErr appError
	if !errors.As(err, &appErr) || appErr.code != exitInvalidConfig {
		t.Fatalf("same primary/replica error = %v, want invalid-config appError", err)
	}
}

func TestCurrentDeviceSnapshotProducerRequiresLocalTrust(t *testing.T) {
	device := state.Device{ID: "dev_self", SigningPublicKey: "age1signing", TrustState: "local"}
	if !currentDeviceIsApprovedSnapshotProducer(device, device.ID) {
		t.Fatal("locally trusted current producer was rejected")
	}
	device.TrustState = "revoked"
	if currentDeviceIsApprovedSnapshotProducer(device, device.ID) {
		t.Fatal("revoked current producer was accepted solely because it had a signing key")
	}
}

func TestMirrorRetentionManifestTreatsAlreadyAdvancedReplicaAsSuccess(t *testing.T) {
	env, store, _ := setupRecovery(t, true)
	defer closeStore(store)
	replica := dssync.FileHub{Path: filepath.Join(t.TempDir(), "replica.json")}
	current := signedRetentionManifest(t, env.wsID, map[string]int64{recoveryProducer: 8}, recoveryProducer, env.prodSign.Private)
	older := signedRetentionManifest(t, env.wsID, map[string]int64{recoveryProducer: 4}, recoveryProducer, env.prodSign.Private)
	if err := replica.PutRetention(env.ctx, current, ""); err != nil {
		t.Fatal(err)
	}
	if err := mirrorRetentionManifest(env.ctx, replica, older); err != nil {
		t.Fatalf("mirror older manifest = %v, want benign concurrent-exporter success", err)
	}
	got, _, err := replica.GetRetention(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, current) {
		t.Fatal("benign no-op changed the already-advanced replica retention head")
	}
}

func TestMirrorRetentionManifestRefusesIncomparableHead(t *testing.T) {
	env, store, _ := setupRecovery(t, true)
	defer closeStore(store)
	replica := dssync.FileHub{Path: filepath.Join(t.TempDir(), "replica.json")}
	current := signedRetentionManifest(t, env.wsID, map[string]int64{recoveryProducer: 8, "dev_other": 2}, recoveryProducer, env.prodSign.Private)
	incomparable := signedRetentionManifest(t, env.wsID, map[string]int64{recoveryProducer: 4, "dev_other": 9}, recoveryProducer, env.prodSign.Private)
	if err := replica.PutRetention(env.ctx, current, ""); err != nil {
		t.Fatal(err)
	}
	if err := mirrorRetentionManifest(env.ctx, replica, incomparable); err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("mirror incomparable manifest = %v, want conflict refusal", err)
	}
	got, _, err := replica.GetRetention(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, current) {
		t.Fatal("conflict refusal changed replica retention head")
	}
}

// TestDurabilityReplicaRestoreDrill exercises disaster recovery end to end:
// compact a synced primary, export its sealed snapshot, delete the entire
// primary carrier, then bootstrap a fresh device directly from the replica's
// snapshot through the production recovery path.
func TestDurabilityReplicaRestoreDrill(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	env, producerStore, _ := setupCompact(t)
	defer closeStore(producerStore)
	if err := hubCompact(env.ctx, io.Discard, io.Discard, env.opts, producerStore, env.hub(t, producerStore), env.hubID, env.paths, 2, 0, true, false); err != nil {
		t.Fatalf("hubCompact: %v", err)
	}
	producer, err := producerStore.CurrentDevice(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	epoch, kid, wck, err := buildKeyring(env.ctx, env.opts, producerStore).PushKey(env.ctx)
	if err != nil || epoch == 0 {
		t.Fatalf("producer PushKey = epoch %d err %v", epoch, err)
	}

	replicaPath := filepath.Join(t.TempDir(), "replica.json")
	env.opts.v.Set("hub_replica", "file:"+replicaPath)
	env.opts.v.Set(durabilityExportConfigKey, "24h")
	if err := maybeExportHubDurability(env.ctx, io.Discard, env.opts, producerStore, env.hub(t, producerStore), env.hubPath, time.Now()); err != nil {
		t.Fatalf("durability export: %v", err)
	}

	// Total primary loss: remove its event carrier, immutable snapshots, and
	// retention metadata. Recovery below has only replicaPath.
	primary := dssync.FileHub{Path: env.hubPath}
	if err := os.RemoveAll(filepath.Dir(env.hubPath)); err != nil {
		t.Fatalf("remove primary hub: %v", err)
	}
	if _, _, err := primary.GetRetention(env.ctx); !errors.Is(err, dssync.ErrRetentionNotFound) {
		t.Fatalf("primary still has retention state after simulated loss: %v", err)
	}

	freshPaths := config.Paths{Home: filepath.Join(t.TempDir(), "fresh-home"), Root: filepath.Join(t.TempDir(), "fresh-root")}
	if err := os.MkdirAll(freshPaths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	freshStore, err := state.Open(env.ctx, freshPaths.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(freshStore)
	if err := freshStore.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := freshStore.EnsureWorkspaceWithID(env.ctx, env.wsID, "restored", freshPaths.Root); err != nil {
		t.Fatal(err)
	}
	freshDevice, err := freshStore.EnsureDevice(env.ctx, "restore-drill")
	if err != nil {
		t.Fatal(err)
	}
	if err := freshStore.RecordKeyCustody(env.ctx, devicekeys.CustodyFile); err != nil {
		t.Fatal(err)
	}
	if err := ensureLocalDeviceIdentity(env.ctx, freshPaths, freshStore, freshDevice); err != nil {
		t.Fatal(err)
	}
	producer.TrustState = "approved"
	if err := freshStore.UpsertDevice(env.ctx, producer); err != nil {
		t.Fatal(err)
	}
	keyStore := devicekeys.NewHybridStore(freshPaths.KeyDir(), keychainBackend()).WithCustody(devicekeys.CustodyFile)
	if err := keyStore.StoreWCK(env.ctx, env.wsID, epoch, kid, wck); err != nil {
		t.Fatal(err)
	}
	if err := freshStore.RecordKeyEpoch(env.ctx, epoch, kid, "grant"); err != nil {
		t.Fatal(err)
	}

	freshV := viper.New()
	freshV.Set("home", freshPaths.Home)
	freshV.Set("root", freshPaths.Root)
	freshV.Set("hub", "file:"+replicaPath)
	freshOpts := &options{v: freshV}
	replicaHub, replicaID, err := hubFromOptions(env.ctx, freshOpts, freshStore, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pullAndApplyEvents(env.ctx, freshStore, replicaHub, replicaID); !errors.Is(err, dssync.ErrSnapshotRequired) {
		t.Fatalf("fresh replica pull = %v, want snapshot bootstrap", err)
	}
	imported, err := recoverFromSnapshot(env.ctx, io.Discard, freshStore, replicaHub, replicaID, freshPaths, buildKeyring(env.ctx, freshOpts, freshStore))
	if err != nil || !imported {
		t.Fatalf("recover from replica = imported %v err %v", imported, err)
	}
	for _, path := range []string{"work/api", "work/web"} {
		if _, err := freshStore.ProjectByPath(env.ctx, path); err != nil {
			t.Fatalf("recovered project %s: %v", path, err)
		}
	}
}
