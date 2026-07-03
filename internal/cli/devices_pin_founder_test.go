package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// P4-SEC-04 (joiner half): the founder-pinning ceremony. A keyless joiner runs
// `devices enroll <founder-id> … --approve` BEFORE its first sync. The grant
// path is founder-gated so the joiner mints and grants nothing, but the
// approved founder row flips hasEnrolledDevices — from that moment
// verifyEventSignature and EncryptedHub.Verify fail closed, so the joiner's
// bootstrap window is closed before it ever pulls from the hub.

type pinnedJoiner struct {
	home           string
	root           string
	opts           *options
	store          *state.Store
	local          state.Device
	founderAge     devicekeys.Identity
	founderSigning devicekeys.SigningIdentity
}

func setupKeylessJoiner(t *testing.T) pinnedJoiner {
	t.Helper()
	t.Setenv(platform.NoKeychainEnv, "1")
	ctx := context.Background()
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--join"); err != nil {
		t.Fatalf("init --join stderr = %q err = %v", stderr, err)
	}
	opts := &options{v: viper.New()}
	opts.v.Set("home", home)
	opts.v.Set("root", root)
	opts.v.Set("role", "joiner")

	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeStore(store) })
	local, err := store.CurrentDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	founderAge, err := devicekeys.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	founderSigning, err := devicekeys.NewSigningIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return pinnedJoiner{
		home: home, root: root, opts: opts, store: store, local: local,
		founderAge: founderAge, founderSigning: founderSigning,
	}
}

func (j pinnedJoiner) pinFounder(t *testing.T) (stdout, stderr string) {
	t.Helper()
	fp, ferr := devicekeys.Fingerprint(j.founderSigning.Public, j.founderAge.Recipient)
	if ferr != nil {
		t.Fatal(ferr)
	}
	var err error
	stdout, stderr, err = executeForTest("--home", j.home, "--root", j.root,
		"devices", "enroll", "dev_founder",
		"--name", "founder", "--os", "darwin", "--arch", "arm64",
		"--age-recipient", j.founderAge.Recipient,
		"--signing-public-key", j.founderSigning.Public,
		"--approve", "--fingerprint", fp)
	if err != nil {
		t.Fatalf("pin founder: stderr = %q err = %v", stderr, err)
	}
	return stdout, stderr
}

func TestKeylessJoinerPinFounderGrantsNothing(t *testing.T) {
	ctx := context.Background()
	j := setupKeylessJoiner(t)

	stdout, stderr := j.pinFounder(t)
	if !strings.Contains(stdout, "enrolled as approved") {
		t.Fatalf("stdout = %q, want enrolled-as-approved confirmation", stdout)
	}
	if !strings.Contains(stderr, "nothing was granted") || !strings.Contains(stderr, "pins that device's keys") {
		t.Fatalf("stderr = %q, want the pinning-ceremony wording", stderr)
	}

	// The joiner minted no epoch and emitted no grant.
	if epoch, err := j.store.CurrentKeyEpoch(ctx); err != nil || epoch != 0 {
		t.Fatalf("epoch = %d, %v — a keyless joiner must not self-mint", epoch, err)
	}
	pending, err := j.store.PendingEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range pending {
		if event.Type == dssync.EventDeviceKeyGranted {
			t.Fatalf("keyless joiner emitted %s: %+v", dssync.EventDeviceKeyGranted, event)
		}
	}
}

// TestUnpinnedJoinerAcceptsForgedGrant is the negative control proving the
// pin is load-bearing: WITHOUT the ceremony, the same forged grant from an
// unknown device ingests fail-open (grants are not in mustVerifyEvent and
// hasEnrolledDevices is false) — the exact P4-SEC-04 window the pin closes.
func TestUnpinnedJoinerAcceptsForgedGrant(t *testing.T) {
	ctx := context.Background()
	j := setupKeylessJoiner(t) // deliberately NOT pinned

	forgedPayload, err := forgedGrantPayload(j.local.PublicKey, 1)
	if err != nil {
		t.Fatal(err)
	}
	forged := state.Event{
		ID: "evt_forged_unpinned", DeviceID: "dev_unknown_attacker", Seq: 1,
		HLC:         time.Now().UTC().UnixMilli() << 16,
		Type:        dssync.EventDeviceKeyGranted,
		PayloadJSON: forgedPayload,
		ContentHash: state.ContentHash(forgedPayload),
	}
	keyring := workspacekeys.New(j.store, devicekeys.NewHybridStore(j.opts.paths().KeyDir(), platform.Detect().Keychain))
	hub := dssync.EncryptedHub{
		Hub:     newSingleEventFileHub(t, forged),
		Keyring: keyring,
		Verify:  j.store.VerifyRemoteEvent,
	}
	pulled, err := hub.Pull(ctx, nil)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if _, err := dssync.ApplyEvents(ctx, j.store, pulled); err != nil {
		t.Fatalf("ApplyEvents: %v", err)
	}
	// Fail-open, as documented: the forged epoch ingests with no conflict. If
	// this ever starts refusing, the bootstrap window has been closed by some
	// new mechanism and the pinning ceremony docs should be revisited.
	if epoch, err := j.store.CurrentKeyEpoch(ctx); err != nil || epoch != 1 {
		t.Fatalf("epoch = %d, %v — expected the pre-pin window to ingest the forged grant (negative control)", epoch, err)
	}
	conflicts, err := j.store.OpenConflictsByType(ctx, dssync.ConflictEventVerification)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want none pre-pin (negative control)", conflicts)
	}
}

func TestPinnedJoinerRefusesForgedGrantBeforeFirstSync(t *testing.T) {
	ctx := context.Background()
	j := setupKeylessJoiner(t)
	j.pinFounder(t)

	// A forged grant from an unknown device, wrapping an attacker-chosen WCK
	// to the joiner's own recipient — the P4-SEC-04 TOFU attack, launched
	// BEFORE the joiner's first sync. Pinning must already fail it closed.
	forgedPayload, err := forgedGrantPayload(j.local.PublicKey, 1)
	if err != nil {
		t.Fatal(err)
	}
	forged := state.Event{
		ID: "evt_forged_prepin", DeviceID: "dev_unknown_attacker", Seq: 1,
		HLC:         time.Now().UTC().UnixMilli() << 16,
		Type:        dssync.EventDeviceKeyGranted,
		PayloadJSON: forgedPayload,
		ContentHash: state.ContentHash(forgedPayload),
	}
	keyring := workspacekeys.New(j.store, devicekeys.NewHybridStore(j.opts.paths().KeyDir(), platform.Detect().Keychain))
	hub := dssync.EncryptedHub{
		Hub:     newSingleEventFileHub(t, forged),
		Keyring: keyring,
		Verify:  j.store.VerifyRemoteEvent,
	}
	pulled, err := hub.Pull(ctx, nil)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if _, err := dssync.ApplyEvents(ctx, j.store, pulled); err != nil {
		t.Fatalf("ApplyEvents: %v", err)
	}

	// Refused: no key ingested, the carrier quarantined.
	if epoch, err := j.store.CurrentKeyEpoch(ctx); err != nil || epoch != 0 {
		t.Fatalf("epoch = %d, %v — forged grant must not ingest on a pinned joiner", epoch, err)
	}
	conflicts, err := j.store.OpenConflictsByType(ctx, dssync.ConflictEventVerification)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 || !bytes.Contains([]byte(conflicts[0].DetailsJSON), []byte(forged.ID)) {
		t.Fatalf("verification conflicts = %+v, want exactly the forged carrier quarantined", conflicts)
	}
}

func TestPinnedJoinerIngestsFounderSignedGrant(t *testing.T) {
	ctx := context.Background()
	j := setupKeylessJoiner(t)
	j.pinFounder(t)

	// The legitimate ceremony continuation: the PINNED founder wraps a real
	// WCK to the joiner's recipient and signs the grant carrier (v2 domain).
	wck, err := dssync.NewWCK()
	if err != nil {
		t.Fatal(err)
	}
	recipient, err := age.ParseX25519Recipient(j.local.PublicKey)
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
	payload, err := json.Marshal(dssync.DeviceKeyGrant{
		Epoch: 1, KID: dssync.KIDForWCK(wck), Recipient: j.local.PublicKey,
		WrappedKey: base64.StdEncoding.EncodeToString(wrapped.Bytes()),
	})
	if err != nil {
		t.Fatal(err)
	}
	grant := state.Event{
		ID: "evt_founder_grant", DeviceID: "dev_founder", Seq: 1,
		HLC:         time.Now().UTC().UnixMilli() << 16,
		Type:        dssync.EventDeviceKeyGranted,
		PayloadJSON: string(payload),
		ContentHash: state.ContentHash(string(payload)),
	}
	sig, err := devicekeys.Sign(j.founderSigning.Private, "devstrap:event:v2", state.EventSignaturePayloadV2(grant))
	if err != nil {
		t.Fatal(err)
	}
	grant.DeviceSig = sig

	keyring := workspacekeys.New(j.store, devicekeys.NewHybridStore(j.opts.paths().KeyDir(), platform.Detect().Keychain))
	hub := dssync.EncryptedHub{
		Hub:     newSingleEventFileHub(t, grant),
		Keyring: keyring,
		Verify:  j.store.VerifyRemoteEvent,
	}
	pulled, err := hub.Pull(ctx, nil)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if _, err := dssync.ApplyEvents(ctx, j.store, pulled); err != nil {
		t.Fatalf("ApplyEvents: %v", err)
	}

	if epoch, err := j.store.CurrentKeyEpoch(ctx); err != nil || epoch != 1 {
		t.Fatalf("epoch = %d, %v — the pinned founder's signed grant must ingest", epoch, err)
	}
	conflicts, err := j.store.OpenConflictsByType(ctx, dssync.ConflictEventVerification)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("verification conflicts = %+v, want none for the founder-signed grant", conflicts)
	}
}

// newSingleEventFileHub writes one raw event into a fresh file-backed hub.
func newSingleEventFileHub(t *testing.T, event state.Event) dssync.FileHub {
	t.Helper()
	raw, err := json.MarshalIndent([]state.Event{event}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "hub.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return dssync.FileHub{Path: path}
}
