package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
	"github.com/Reederey87/DevStrap/internal/workspacekeys"
	"github.com/spf13/viper"
)

// P6-SEC-03 end-to-end at the pull/apply layer: an event sealed under an
// epoch this device was never granted stops truncating once the grace window
// (0 here) expires — it quarantines as an undecryptable conflict, the cursor
// advances (sync is unwedged), the wait is visible to doctor — and when the
// grant finally arrives, the SAME sync cycle recovers the carrier via
// ReplayUndecryptableConflicts (which now runs BEFORE the batch applies),
// resolves the conflict, and clears the wait.
func TestSyncQuarantinesNeverGrantedEpochThenRecovers(t *testing.T) {
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
	defer closeStore(store)
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureWorkspace(ctx, "victim", root); err != nil {
		t.Fatal(err)
	}
	local, err := store.EnsureDevice(ctx, "victim")
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureLocalDeviceIdentity(ctx, paths, store, local); err != nil {
		t.Fatal(err)
	}
	local, err = store.CurrentDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}

	remoteAge, err := devicekeys.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	remoteSigning, err := devicekeys.NewSigningIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertDevice(ctx, state.Device{
		ID: "dev_rotator", Name: "rotator", OS: "linux", Arch: "arm64",
		PublicKey: remoteAge.Recipient, SigningPublicKey: remoteSigning.Public, TrustState: "approved",
	}); err != nil {
		t.Fatal(err)
	}

	keyring := workspacekeys.New(store, devicekeys.NewHybridStore(paths.KeyDir(), platform.Detect().Keychain))
	if epoch, err := keyring.EnsureBootstrap(ctx); err != nil || epoch != 1 {
		t.Fatalf("EnsureBootstrap = %d, %v; want epoch 1", epoch, err)
	}

	// The rotator minted epoch 2 and sealed a project event under it, but its
	// grant to this device is NOT on the hub yet (the P6-SEC-03 wedge).
	wck2, err := dssync.NewWCK()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(dssync.ProjectPayload{Path: "work/acme/wedged", Type: "git_repo", RemoteKey: "github.com/acme/wedged"})
	if err != nil {
		t.Fatal(err)
	}
	plain := state.Event{
		ID: "evt_epoch2_project", DeviceID: "dev_rotator", Seq: 1,
		HLC:         time.Now().UTC().UnixMilli() << 16,
		Type:        dssync.EventProjectAdded,
		PayloadJSON: string(payload),
		ContentHash: state.ContentHash(string(payload)),
	}
	sig, err := devicekeys.Sign(remoteSigning.Private, "devstrap:event:v1", state.EventSignaturePayload(plain))
	if err != nil {
		t.Fatal(err)
	}
	plain.DeviceSig = sig
	sealed, err := dssync.EncryptEvent(plain, wck2, 2)
	if err != nil {
		t.Fatal(err)
	}

	hubPath := filepath.Join(t.TempDir(), "hub.json")
	writeHub := func(events []state.Event) {
		t.Helper()
		raw, err := json.MarshalIndent(events, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(hubPath, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeHub([]state.Event{sealed})

	hub := dssync.EncryptedHub{
		Hub:            dssync.FileHub{Path: hubPath},
		Keyring:        keyring,
		Verify:         store.VerifyRemoteEvent,
		Stats:          &dssync.PullStats{},
		MissingKeyWait: store.NoteMissingKeyGrant,
		GraceWindow:    0, // expire immediately: quarantine on first sight
	}
	hubID := "file:" + hubPath

	// Cycle 1: the never-granted epoch-2 event quarantines instead of
	// truncating — the cursor advances past it and a wait opens.
	if _, err := pullAndApplyEvents(ctx, store, hub, hubID); err != nil {
		t.Fatalf("pull cycle 1: %v", err)
	}
	if hub.Stats.Undecryptable != 1 || hub.Stats.Truncated != 0 {
		t.Fatalf("Stats after cycle 1 = %+v, want Undecryptable=1 Truncated=0", *hub.Stats)
	}
	cursor, err := store.HubCursor(ctx, hubID)
	if err != nil {
		t.Fatal(err)
	}
	if cursor < sealed.HLC {
		t.Fatalf("cursor = %d, want advanced past the quarantined event's HLC %d (the un-wedge)", cursor, sealed.HLC)
	}
	open, err := store.OpenConflictsByType(ctx, dssync.ConflictEventVerification)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("open conflicts after cycle 1 = %d, want the undecryptable quarantine", len(open))
	}
	waits, err := store.OpenKeyGrantWaits(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(waits) != 1 || waits[0].Epoch != 2 {
		t.Fatalf("waits after cycle 1 = %+v, want one epoch-2 wait", waits)
	}

	// The rotator finally grants epoch 2 to this device (verified carrier).
	ageRecipient, err := age.ParseX25519Recipient(local.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	var wrapped bytes.Buffer
	w, err := age.Encrypt(&wrapped, ageRecipient)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(wck2); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	grantPayload, err := json.Marshal(dssync.DeviceKeyGrant{
		Epoch:      2,
		KID:        dssync.KIDForWCK(wck2),
		Recipient:  local.PublicKey,
		WrappedKey: base64.StdEncoding.EncodeToString(wrapped.Bytes()),
	})
	if err != nil {
		t.Fatal(err)
	}
	grant := state.Event{
		ID: "evt_epoch2_grant", DeviceID: "dev_rotator", Seq: 2,
		HLC:         time.Now().UTC().UnixMilli()<<16 + 1,
		Type:        dssync.EventDeviceKeyGranted,
		PayloadJSON: string(grantPayload),
		ContentHash: state.ContentHash(string(grantPayload)),
	}
	grantSig, err := devicekeys.Sign(remoteSigning.Private, "devstrap:event:v1", state.EventSignaturePayload(grant))
	if err != nil {
		t.Fatal(err)
	}
	grant.DeviceSig = grantSig
	writeHub([]state.Event{sealed, grant})

	// Cycle 2: the pull ingests the grant, and the replay (running BEFORE the
	// batch applies) recovers the quarantined carrier in the SAME cycle.
	if _, err := pullAndApplyEvents(ctx, store, hub, hubID); err != nil {
		t.Fatalf("pull cycle 2: %v", err)
	}
	if _, err := store.ProjectByPath(ctx, "work/acme/wedged"); err != nil {
		t.Fatalf("recovered project missing after grant: %v", err)
	}
	got, err := store.ConflictByID(ctx, open[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "resolved" {
		t.Fatalf("conflict status = %q, want resolved after recovery", got.Status)
	}
	waits, err = store.OpenKeyGrantWaits(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(waits) != 0 {
		t.Fatalf("waits after recovery = %+v, want none (RecordKeyEpoch cleared them)", waits)
	}
}
