package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"testing"

	"filippo.io/age"
	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
	"github.com/spf13/viper"
)

// TestReplayIngestsQuarantinedGrant pins the post-#33 review hardening: a
// device.key.granted carrier that was quarantined (its author was not yet
// approved locally) is only ever WCK-ingested by EncryptedHub.Pull, which has
// already advanced past it and never re-pulls. The approve-time replay must
// therefore ingest the grant into the keyring — otherwise the granted
// (epoch, kid) is permanently lost and every fleet event sealed under it
// defers forever.
func TestReplayIngestsQuarantinedGrant(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	ctx := context.Background()
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--workspace-name", "personal"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	opts := &options{v: viper.New()}
	opts.v.Set("home", home)
	opts.v.Set("root", root)

	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	local, err := store.CurrentDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// An approved non-local device puts the workspace in the fail-closed
	// regime (hasEnrolledDevices), so the pending granter's event quarantines.
	otherSigning, err := devicekeys.NewSigningIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertDevice(ctx, state.Device{
		ID: "dev_other_approved", Name: "other", OS: "linux", Arch: "arm64",
		SigningPublicKey: otherSigning.Public, TrustState: "approved",
	}); err != nil {
		t.Fatal(err)
	}

	// The granter is known but only pending, so its signed grant quarantines
	// under the fail-closed regime.
	granterSigning, err := devicekeys.NewSigningIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertDevice(ctx, state.Device{
		ID: "dev_granter", Name: "granter", OS: "linux", Arch: "arm64",
		SigningPublicKey: granterSigning.Public, TrustState: "pending",
	}); err != nil {
		t.Fatal(err)
	}

	// A real grant: a fresh WCK age-wrapped to the LOCAL device's recipient.
	wck, err := dssync.NewWCK()
	if err != nil {
		t.Fatal(err)
	}
	recipient, err := age.ParseX25519Recipient(local.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	var wrapped bytes.Buffer
	w, err := age.Encrypt(&wrapped, recipient)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(wck); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	kid := dssync.KIDForWCK(wck)
	payload, err := json.Marshal(dssync.DeviceKeyGrant{
		Epoch: 1, KID: kid, Recipient: local.PublicKey,
		WrappedKey: base64.StdEncoding.EncodeToString(wrapped.Bytes()),
	})
	if err != nil {
		t.Fatal(err)
	}
	event := state.Event{
		ID: "evt_grant_quarantined", DeviceID: "dev_granter", Seq: 1, HLC: 10 << 16,
		Type: dssync.EventDeviceKeyGranted, PayloadJSON: string(payload),
		ContentHash: state.ContentHash(string(payload)),
	}
	sig, err := devicekeys.Sign(granterSigning.Private, "devstrap:event:v1", state.EventSignaturePayload(event))
	if err != nil {
		t.Fatal(err)
	}
	event.DeviceSig = sig

	if _, err := dssync.ApplyEvents(ctx, store, []state.Event{event}); err != nil {
		t.Fatalf("quarantine grant event: %v", err)
	}
	open, err := store.OpenConflictsByType(ctx, dssync.ConflictEventVerification)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("open verification conflicts = %d, want 1", len(open))
	}
	if epoch, err := store.CurrentKeyEpoch(ctx); err != nil || epoch != 0 {
		t.Fatalf("epoch before replay = %d/%v, want 0/nil", epoch, err)
	}

	if err := store.SetDeviceTrustState(ctx, "dev_granter", "approved"); err != nil {
		t.Fatal(err)
	}
	var replayErr bytes.Buffer
	replayQuarantinedEvents(ctx, &replayErr, opts, store, "dev_granter")

	// The grant's key is now actually held: metadata, custody, and decrypt path.
	if epoch, err := store.CurrentKeyEpoch(ctx); err != nil || epoch != 1 {
		t.Fatalf("epoch after replay = %d/%v (stderr %q), want 1/nil", epoch, err, replayErr.String())
	}
	kr := buildKeyring(opts, store)
	if err := kr.Prime(ctx); err != nil {
		t.Fatalf("Prime after replay: %v", err)
	}
	got := kr.WCKCandidates(1, kid)
	if len(got) != 1 || string(got[0]) != string(wck) {
		t.Fatalf("replayed grant WCK not held for (1, %s)", kid)
	}
	conflict, err := store.ConflictByID(ctx, open[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if conflict.Status != "resolved" {
		t.Fatalf("conflict status = %q, want resolved", conflict.Status)
	}
}
