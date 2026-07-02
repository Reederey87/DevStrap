package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
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

func TestSyncRejectsForgedGrantBeforeWCKIngest(t *testing.T) {
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
		ID: "dev_approved_remote", Name: "approved remote", OS: "linux", Arch: "arm64",
		PublicKey: remoteAge.Recipient, SigningPublicKey: remoteSigning.Public, TrustState: "approved",
	}); err != nil {
		t.Fatal(err)
	}

	keyring := workspacekeys.New(store, devicekeys.NewHybridStore(paths.KeyDir(), platform.Detect().Keychain))
	if epoch, err := keyring.EnsureBootstrap(ctx); err != nil {
		t.Fatalf("EnsureBootstrap: %v", err)
	} else if epoch != 1 {
		t.Fatalf("bootstrap epoch = %d, want 1", epoch)
	}
	beforeEpoch, err := store.CurrentKeyEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if beforeEpoch != 1 {
		t.Fatalf("current epoch before forged sync = %d, want 1", beforeEpoch)
	}

	const forgedEpoch int64 = 1 << 40
	forgedPayload, err := forgedGrantPayload(local.PublicKey, forgedEpoch)
	if err != nil {
		t.Fatal(err)
	}
	forged := state.Event{
		ID: "evt_forged_grant", DeviceID: "dev_unknown_attacker", Seq: 1,
		HLC:         time.Now().UTC().UnixMilli() << 16,
		Type:        dssync.EventDeviceKeyGranted,
		PayloadJSON: forgedPayload,
		ContentHash: state.ContentHash(forgedPayload),
	}
	hubPath := filepath.Join(t.TempDir(), "hub.json")
	raw, err := json.MarshalIndent([]state.Event{forged}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hubPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	hub := dssync.EncryptedHub{
		Hub:     dssync.FileHub{Path: hubPath},
		Keyring: keyring,
		Verify:  store.VerifyRemoteEvent,
	}
	pulled, err := hub.Pull(ctx, 0)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(pulled) != 1 || pulled[0].ID != forged.ID {
		t.Fatalf("Pull returned %+v, want forged carrier passthrough", pulled)
	}
	if _, err := dssync.ApplyEvents(ctx, store, pulled); err != nil {
		t.Fatalf("ApplyEvents: %v", err)
	}

	afterEpoch, err := store.CurrentKeyEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if afterEpoch != beforeEpoch {
		t.Fatalf("current epoch changed to %d, want %d", afterEpoch, beforeEpoch)
	}
	workspaceID, err := store.WorkspaceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	forgedWCKPath := filepath.Join(paths.KeyDir(), "wck-"+workspaceID+"-"+strconv.FormatInt(forgedEpoch, 10)+".key")
	if _, err := os.Stat(forgedWCKPath); !os.IsNotExist(err) {
		t.Fatalf("forged WCK file stat error = %v, want not exist at %s", err, forgedWCKPath)
	}
	conflicts, err := store.OpenConflictsByType(ctx, dssync.ConflictEventVerification)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("verification conflicts = %+v, want exactly one", conflicts)
	}
	if !bytes.Contains([]byte(conflicts[0].DetailsJSON), []byte(forged.ID)) {
		t.Fatalf("conflict details = %s, want forged event id", conflicts[0].DetailsJSON)
	}
}

func forgedGrantPayload(recipient string, epoch int64) (string, error) {
	wck := make([]byte, 32)
	if _, err := rand.Read(wck); err != nil {
		return "", err
	}
	ageRecipient, err := age.ParseX25519Recipient(recipient)
	if err != nil {
		return "", err
	}
	var wrapped bytes.Buffer
	writer, err := age.Encrypt(&wrapped, ageRecipient)
	if err != nil {
		return "", err
	}
	if _, err := writer.Write(wck); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	payload, err := json.Marshal(dssync.DeviceKeyGrant{
		Epoch:      epoch,
		Recipient:  recipient,
		WrappedKey: base64.StdEncoding.EncodeToString(wrapped.Bytes()),
	})
	if err != nil {
		return "", err
	}
	return string(payload), nil
}
