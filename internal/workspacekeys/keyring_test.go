package workspacekeys

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
)

type memSecret struct{ values map[string][]byte }

func TestMain(m *testing.M) {
	_ = os.Setenv(platform.NoKeychainEnv, "1")
	os.Exit(m.Run())
}

func (b *memSecret) Store(_ context.Context, service, account string, secret []byte) error {
	if b.values == nil {
		b.values = map[string][]byte{}
	}
	b.values[service+"/"+account] = append([]byte(nil), secret...)
	return nil
}
func (b *memSecret) Load(_ context.Context, service, account string) ([]byte, error) {
	v, ok := b.values[service+"/"+account]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), v...), nil
}
func (b *memSecret) Delete(_ context.Context, service, account string) error {
	delete(b.values, service+"/"+account)
	return nil
}

// setupKeyring opens a store, migrates it, ensures a workspace + local device,
// creates the device age identity, and returns a primed Keyring.
func setupKeyring(t *testing.T, name string) (*state.Store, *Keyring, devicekeys.Identity) {
	st, kr, identity, _ := setupKeyringWithDBPath(t, name)
	return st, kr, identity
}

func setupKeyringWithDBPath(t *testing.T, name string) (*state.Store, *Keyring, devicekeys.Identity, string) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	st, err := state.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(ctx, name, "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	device, err := st.EnsureDevice(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	keyStore := devicekeys.NewHybridStore(t.TempDir(), &memSecret{})
	identity, _, err := keyStore.Ensure(ctx, device.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetDevicePublicKey(ctx, device.ID, identity.Recipient); err != nil {
		t.Fatal(err)
	}
	return st, New(st, keyStore), identity, dbPath
}

func TestEnsureBootstrapMintsFirstEpoch(t *testing.T) {
	ctx := context.Background()
	st, kr, _ := setupKeyring(t, "a")

	epoch, err := kr.EnsureBootstrap(ctx)
	if err != nil {
		t.Fatalf("EnsureBootstrap: %v", err)
	}
	if epoch != 1 {
		t.Fatalf("epoch = %d, want 1", epoch)
	}
	got, err := st.CurrentKeyEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("stored epoch = %d, want 1", got)
	}
	wck, ok := kr.WCK(1)
	if !ok {
		t.Fatal("WCK(1) missing after bootstrap")
	}
	if len(wck) != wckSize {
		t.Fatalf("WCK length = %d, want %d", len(wck), wckSize)
	}
	// EnsureBootstrap is idempotent: a second call does not mint a new epoch.
	if e2, err := kr.EnsureBootstrap(ctx); err != nil || e2 != 1 {
		t.Fatalf("second EnsureBootstrap = %d/%v, want 1/nil", e2, err)
	}
}

func TestSelfGrantAndIngestRoundTrip(t *testing.T) {
	ctx := context.Background()
	_, kr, idA := setupKeyring(t, "a")
	if _, err := kr.EnsureBootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	wck1, _ := kr.WCK(1)

	// Emit a self-grant for epoch 1 (the bootstrap self-grant init emits).
	events, err := kr.GrantAllEpochs(ctx, idA.Recipient)
	if err != nil {
		t.Fatalf("GrantAllEpochs: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("GrantAllEpochs emitted %d events, want 1", len(events))
	}
	var grant dssync.DeviceKeyGrant
	if err := json.Unmarshal([]byte(events[0].PayloadJSON), &grant); err != nil {
		t.Fatal(err)
	}
	if grant.Epoch != 1 || grant.Recipient != idA.Recipient {
		t.Fatalf("grant = %+v, want epoch 1 recipient %s", grant, idA.Recipient)
	}
	// The grant payload must not leak the raw WCK.
	raw, _ := json.Marshal(grant)
	if contains(string(raw), string(wck1)) {
		t.Fatal("grant payload leaked the raw WCK")
	}

	// Simulate losing the in-memory cache and re-ingesting the grant.
	kr2 := &Keyring{Store: kr.Store, KeyStore: kr.KeyStore}
	if err := kr2.IngestGrant(ctx, grant); err != nil {
		t.Fatalf("IngestGrant: %v", err)
	}
	reloaded, ok := kr2.WCK(1)
	if !ok {
		t.Fatal("WCK(1) missing after re-ingest")
	}
	if string(reloaded) != string(wck1) {
		t.Fatalf("re-ingested WCK differs from original")
	}
}

func TestGrantEventAndAuditRowAtomic(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	ctx := context.Background()
	_, kr, idA, dbPath := setupKeyringWithDBPath(t, "a")
	if _, err := kr.EnsureBootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	events, err := kr.GrantAllEpochs(ctx, idA.Recipient)
	if err != nil {
		t.Fatalf("GrantAllEpochs: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("GrantAllEpochs emitted %d events, want 1", len(events))
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: dbPath}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var grantRows int
	err = db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM workspace_key_grants
WHERE epoch = 1 AND recipient = ? AND source_event_id = ? AND source_event_hlc = ? AND source_event_device_id = ?;
`, idA.Recipient, events[0].ID, events[0].HLC, events[0].DeviceID).Scan(&grantRows)
	if err != nil {
		t.Fatal(err)
	}
	if grantRows != 1 {
		t.Fatalf("workspace_key_grants matching event = %d, want 1", grantRows)
	}
	var eventRows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE id = ?;`, events[0].ID).Scan(&eventRows); err != nil {
		t.Fatal(err)
	}
	if eventRows != 1 {
		t.Fatalf("events matching grant = %d, want 1", eventRows)
	}
}

func TestIngestGrantNoOpForOtherRecipient(t *testing.T) {
	ctx := context.Background()
	_, kr, _ := setupKeyring(t, "a")
	if _, err := kr.EnsureBootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	// A grant addressed to a different recipient is a no-op.
	other, err := devicekeys.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	wck, _ := kr.WCK(1)
	wrapped, err := wrapWCK(wck, other.Recipient)
	if err != nil {
		t.Fatal(err)
	}
	grant := dssync.DeviceKeyGrant{Epoch: 1, Recipient: other.Recipient, WrappedKey: wrapped}
	fresh := &Keyring{Store: kr.Store, KeyStore: kr.KeyStore}
	if err := fresh.resolve(ctx); err != nil {
		t.Fatal(err)
	}
	if err := fresh.IngestGrant(ctx, grant); err != nil {
		t.Fatalf("IngestGrant for other recipient: %v", err)
	}
	if _, ok := fresh.WCK(1); ok {
		t.Fatal("IngestGrant for another recipient should not populate WCK")
	}
}

func TestRotateMintsNextEpochAndExcludesRevoked(t *testing.T) {
	ctx := context.Background()
	st, kr, idA := setupKeyring(t, "a")
	if _, err := kr.EnsureBootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	// Enroll an approved remote device B, then revoke it so ApprovedRecipients
	// excludes it when Rotate runs.
	bID, err := devicekeys.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertDevice(ctx, state.Device{
		ID: "dev_b", Name: "b", OS: "linux", Arch: "arm64",
		PublicKey: bID.Recipient, TrustState: "approved",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeviceTrustState(ctx, "dev_b", "revoked"); err != nil {
		t.Fatal(err)
	}
	next, events, err := kr.Rotate(ctx)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if next != 2 {
		t.Fatalf("next epoch = %d, want 2", next)
	}
	// Grants go to the remaining approved recipients (the local device only,
	// since B is revoked). No grant for B.
	for _, ev := range events {
		var g dssync.DeviceKeyGrant
		if err := json.Unmarshal([]byte(ev.PayloadJSON), &g); err != nil {
			t.Fatal(err)
		}
		if g.Recipient == bID.Recipient {
			t.Fatal("Rotate granted to revoked device B")
		}
		if g.Epoch != 2 {
			t.Fatalf("rotated grant epoch = %d, want 2", g.Epoch)
		}
	}
	if _, ok := kr.WCK(2); !ok {
		t.Fatal("WCK(2) missing after rotate")
	}
	// Forward secrecy: WCK(2) differs from WCK(1).
	w2, _ := kr.WCK(2)
	w1, _ := kr.WCK(1)
	if string(w2) == string(w1) {
		t.Fatal("rotated WCK equals previous epoch WCK")
	}
	// The local device (idA) received an epoch-2 grant it can re-ingest.
	var localGrant dssync.DeviceKeyGrant
	for _, ev := range events {
		var g dssync.DeviceKeyGrant
		_ = json.Unmarshal([]byte(ev.PayloadJSON), &g)
		if g.Recipient == idA.Recipient {
			localGrant = g
		}
	}
	if localGrant.WrappedKey == "" {
		t.Fatal("no epoch-2 grant for the local device")
	}
	fresh := &Keyring{Store: kr.Store, KeyStore: kr.KeyStore}
	if err := fresh.IngestGrant(ctx, localGrant); err != nil {
		t.Fatalf("re-ingest rotated grant: %v", err)
	}
	fw2, _ := fresh.WCK(2)
	if string(fw2) != string(w2) {
		t.Fatal("re-ingested rotated WCK differs")
	}
}

// TestNewDeviceReadsHistoryAcrossEpochBump proves the core zero-knowledge
// property: device A encrypts events under epoch 1 and (after a rotate) epoch 2;
// newly-approved device B ingests A's grants and decrypts both epochs' events.
func TestNewDeviceReadsHistoryAcrossEpochBump(t *testing.T) {
	ctx := context.Background()
	stA, krA, _ := setupKeyring(t, "a")

	// A bootstraps epoch 1 and encrypts an event under it.
	if _, err := krA.EnsureBootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	wck1, _ := krA.WCK(1)
	enc1, err := dssync.EncryptEvent(state.Event{ID: "evt_e1", Type: dssync.EventProjectAdded, PayloadJSON: `{"path":"work/secret"}`, ContentHash: state.ContentHash(`{"path":"work/secret"}`)}, wck1, 1)
	if err != nil {
		t.Fatal(err)
	}

	// A rotates to epoch 2 (B is not yet approved, so the epoch-2 grant goes to
	// A only) and encrypts a go-forward event under it.
	if _, _, err := krA.Rotate(ctx); err != nil {
		t.Fatal(err)
	}
	wck2, _ := krA.WCK(2)
	enc2, err := dssync.EncryptEvent(state.Event{ID: "evt_e2", Type: dssync.EventProjectAdded, PayloadJSON: `{"path":"work/secret2"}`, ContentHash: state.ContentHash(`{"path":"work/secret2"}`)}, wck2, 2)
	if err != nil {
		t.Fatal(err)
	}

	// B comes online as its own device with its own age identity.
	_, krB, idB := setupKeyring(t, "b")
	bRecipient := idB.Recipient
	// Enroll B as approved on A's store, then A grants ALL held epochs (1 and 2)
	// to B so B can decrypt the whole history.
	if err := stA.UpsertDevice(ctx, state.Device{
		ID: "dev_b", Name: "b", OS: "linux", Arch: "arm64",
		PublicKey: bRecipient, SigningPublicKey: "ed25519:fake", TrustState: "approved",
	}); err != nil {
		t.Fatal(err)
	}
	grantEvents, err := krA.GrantAllEpochs(ctx, bRecipient)
	if err != nil {
		t.Fatalf("GrantAllEpochs to B: %v", err)
	}
	if len(grantEvents) != 2 {
		t.Fatalf("expected 2 grants for B (epochs 1,2), got %d", len(grantEvents))
	}

	if err := krB.Prime(ctx); err != nil {
		t.Fatalf("B Prime: %v", err)
	}
	// B ingests the grants from A (epochs 1 and 2).
	for _, ev := range grantEvents {
		var g dssync.DeviceKeyGrant
		if err := json.Unmarshal([]byte(ev.PayloadJSON), &g); err != nil {
			t.Fatal(err)
		}
		if err := krB.IngestGrant(ctx, g); err != nil {
			t.Fatalf("B IngestGrant epoch %d: %v", g.Epoch, err)
		}
	}
	bwck1, ok := krB.WCK(1)
	if !ok || string(bwck1) != string(wck1) {
		t.Fatal("B did not recover epoch-1 WCK")
	}
	bwck2, ok := krB.WCK(2)
	if !ok || string(bwck2) != string(wck2) {
		t.Fatal("B did not recover epoch-2 WCK")
	}
	// B decrypts both epochs' events.
	dec1, err := dssync.DecryptEvent(enc1, bwck1)
	if err != nil {
		t.Fatalf("B decrypt epoch 1: %v", err)
	}
	if dec1.Type != dssync.EventProjectAdded || dec1.PayloadJSON != `{"path":"work/secret"}` {
		t.Fatalf("B decrypt epoch 1 = %+v", dec1)
	}
	if _, err := dssync.DecryptEvent(enc2, bwck2); err != nil {
		t.Fatalf("B decrypt epoch 2: %v", err)
	}
}

func TestIngestGrantRejectsTamperedWrappedKey(t *testing.T) {
	ctx := context.Background()
	_, kr, idA := setupKeyring(t, "a")
	if _, err := kr.EnsureBootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	wck, _ := kr.WCK(1)
	wrapped, err := wrapWCK(wck, idA.Recipient)
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt the wrapped key: age.Decrypt must fail.
	tampered := wrapped[:len(wrapped)-4] + "AAAA"
	grant := dssync.DeviceKeyGrant{Epoch: 1, Recipient: idA.Recipient, WrappedKey: tampered}
	fresh := &Keyring{Store: kr.Store, KeyStore: kr.KeyStore}
	if err := fresh.IngestGrant(ctx, grant); err == nil {
		t.Fatal("IngestGrant with tampered wrapped key unexpectedly succeeded")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// TestSameEpochKeysCoexistAndPushPrefersGrant pins the P6-SEC-02 / P6-SEC-01(b)
// collision model: a locally self-minted key and a fleet key granted at the
// SAME epoch coexist in the keyring — ingesting the grant never overwrites the
// self-mint — and PushKey selects the fleet (grant-origin) key so the device
// converges onto what the rest of the fleet encrypts under.
func TestSameEpochKeysCoexistAndPushPrefersGrant(t *testing.T) {
	ctx := context.Background()
	_, kr, idA := setupKeyring(t, "a")
	// Legacy joiner behavior: the device self-minted epoch 1.
	if _, err := kr.EnsureBootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	selfWCK, ok := kr.WCK(1)
	if !ok {
		t.Fatal("self-minted WCK missing")
	}
	// The founder's fleet key arrives as a grant at the same epoch.
	fleetWCK, err := dssync.NewWCK()
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := wrapWCK(fleetWCK, idA.Recipient)
	if err != nil {
		t.Fatal(err)
	}
	fleetKID := dssync.KIDForWCK(fleetWCK)
	grant := dssync.DeviceKeyGrant{Epoch: 1, KID: fleetKID, Recipient: idA.Recipient, WrappedKey: wrapped}
	if err := kr.IngestGrant(ctx, grant); err != nil {
		t.Fatalf("IngestGrant fleet key: %v", err)
	}
	// Both keys are held: decrypt candidates for each kid resolve.
	selfKID := dssync.KIDForWCK(selfWCK)
	if got := kr.WCKCandidates(1, selfKID); len(got) != 1 || string(got[0]) != string(selfWCK) {
		t.Fatal("self-minted key lost after fleet grant (overwrite!)")
	}
	if got := kr.WCKCandidates(1, fleetKID); len(got) != 1 || string(got[0]) != string(fleetWCK) {
		t.Fatal("fleet key not held after grant")
	}
	// PushKey prefers the fleet key.
	epoch, kid, wck, err := kr.PushKey(ctx)
	if err != nil {
		t.Fatalf("PushKey: %v", err)
	}
	if epoch != 1 || kid != fleetKID || string(wck) != string(fleetWCK) {
		t.Fatalf("PushKey = (%d, %s), want the fleet key (1, %s)", epoch, kid, fleetKID)
	}
	// A cold keyring (fresh cache) reaches the same selection from persisted state.
	cold := &Keyring{Store: kr.Store, KeyStore: kr.KeyStore}
	epoch, kid, wck, err = cold.PushKey(ctx)
	if err != nil {
		t.Fatalf("cold PushKey: %v", err)
	}
	if epoch != 1 || kid != fleetKID || string(wck) != string(fleetWCK) {
		t.Fatalf("cold PushKey = (%d, %s), want the fleet key (1, %s)", epoch, kid, fleetKID)
	}
}

// TestIngestGrantRejectsKidMismatch proves a grant whose carried kid disagrees
// with its unwrapped bytes is refused (forged or corrupted grant, P6-SEC-02).
func TestIngestGrantRejectsKidMismatch(t *testing.T) {
	ctx := context.Background()
	_, kr, idA := setupKeyring(t, "a")
	wck, err := dssync.NewWCK()
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := wrapWCK(wck, idA.Recipient)
	if err != nil {
		t.Fatal(err)
	}
	forgedKID := strings.Repeat("0123456789abcdef", 4) // well-formed but wrong
	grant := dssync.DeviceKeyGrant{Epoch: 1, KID: forgedKID, Recipient: idA.Recipient, WrappedKey: wrapped}
	if grant.KID == dssync.KIDForWCK(wck) {
		t.Fatal("test setup: forged kid accidentally matches")
	}
	if err := kr.IngestGrant(ctx, grant); err == nil {
		t.Fatal("IngestGrant with mismatched kid unexpectedly succeeded")
	}
	if _, ok := kr.WCK(1); ok {
		t.Fatal("mismatched-kid grant still installed a key")
	}
}

// TestPushKeyEmptyKeyringReturnsZero pins the joiner posture: no held keys
// means PushKey reports epoch 0 with no error, and EncryptedHub.Push turns
// that into ErrMissingWorkspaceKey.
func TestPushKeyEmptyKeyringReturnsZero(t *testing.T) {
	ctx := context.Background()
	_, kr, _ := setupKeyring(t, "a")
	epoch, kid, wck, err := kr.PushKey(ctx)
	if err != nil {
		t.Fatalf("PushKey: %v", err)
	}
	if epoch != 0 || kid != "" || wck != nil {
		t.Fatalf("PushKey on empty keyring = (%d, %q, %v), want (0, \"\", nil)", epoch, kid, wck)
	}
}

// TestPrimeUpgradesLegacyKidlessKey proves the migration-00014 lazy backfill:
// a pre-kid key recorded with kid=” (legacy custody slot, legacy metadata row)
// is upgraded on Prime — kid computed from the bytes, key re-stored under the
// kid-aware slot, metadata row rewritten — and both kid-addressed and legacy
// kid-less envelopes then decrypt.
func TestPrimeUpgradesLegacyKidlessKey(t *testing.T) {
	ctx := context.Background()
	st, kr, _ := setupKeyring(t, "a")
	if err := kr.resolve(ctx); err != nil {
		t.Fatal(err)
	}
	// Simulate a pre-kid install: key in the legacy custody slot, metadata row
	// with kid='' and origin 'legacy' (what migration 00014 backfills).
	legacyWCK, err := dssync.NewWCK()
	if err != nil {
		t.Fatal(err)
	}
	if err := kr.KeyStore.StoreWCK(ctx, kr.workspaceID, 1, "", legacyWCK); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordKeyEpoch(ctx, 1, "", "legacy"); err != nil {
		t.Fatal(err)
	}

	if err := kr.Prime(ctx); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	kid := dssync.KIDForWCK(legacyWCK)
	held, err := st.HeldKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 1 || held[0].KID != kid || held[0].Origin != "legacy" {
		t.Fatalf("held keys after Prime = %+v, want kid %s origin legacy", held, kid)
	}
	if got := kr.WCKCandidates(1, kid); len(got) != 1 || string(got[0]) != string(legacyWCK) {
		t.Fatal("kid-addressed lookup missing after legacy upgrade")
	}
	if got := kr.WCKCandidates(1, ""); len(got) != 1 || string(got[0]) != string(legacyWCK) {
		t.Fatal("legacy kid-less lookup missing after upgrade")
	}
	// The upgraded key round-trips through the kid-aware custody slot on a cold
	// keyring.
	cold := &Keyring{Store: kr.Store, KeyStore: kr.KeyStore}
	if err := cold.Prime(ctx); err != nil {
		t.Fatalf("cold Prime after upgrade: %v", err)
	}
	if got, ok := cold.WCK(1); !ok || string(got) != string(legacyWCK) {
		t.Fatal("cold keyring did not load the upgraded key")
	}
}

// TestConcurrentSameEpochRotateNoClobber simulates two devices rotating to the
// same epoch independently (the concurrent-revoke race): both keys coexist
// under their own kids once the second device's grant is ingested, instead of
// one clobbering the other.
func TestConcurrentSameEpochRotateNoClobber(t *testing.T) {
	ctx := context.Background()
	_, krA, idA := setupKeyring(t, "a")
	if _, err := krA.EnsureBootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	// A rotates to epoch 2.
	if next, _, err := krA.Rotate(ctx); err != nil || next != 2 {
		t.Fatalf("Rotate = %d/%v, want 2/nil", next, err)
	}
	aWCK2, ok := krA.WCK(2)
	if !ok {
		t.Fatal("A missing its rotated key")
	}
	// A concurrent rotate on another device minted a DIFFERENT epoch-2 key and
	// granted it to A.
	otherWCK2, err := dssync.NewWCK()
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := wrapWCK(otherWCK2, idA.Recipient)
	if err != nil {
		t.Fatal(err)
	}
	grant := dssync.DeviceKeyGrant{Epoch: 2, KID: dssync.KIDForWCK(otherWCK2), Recipient: idA.Recipient, WrappedKey: wrapped}
	if err := krA.IngestGrant(ctx, grant); err != nil {
		t.Fatalf("IngestGrant concurrent rotate: %v", err)
	}
	if got := krA.WCKCandidates(2, dssync.KIDForWCK(aWCK2)); len(got) != 1 {
		t.Fatal("A's own rotated key was clobbered by the concurrent grant")
	}
	if got := krA.WCKCandidates(2, dssync.KIDForWCK(otherWCK2)); len(got) != 1 {
		t.Fatal("the concurrently-rotated fleet key was not ingested alongside")
	}
}

// TestPrimeRefusesLegacyUpgradeOverDifferentBytes pins the post-#33 review
// hardening: the legacy-backfill upgrade in Prime must never displace
// different bytes already sitting in the kid-aware custody slot (that would
// mean local corruption or tampering — overwriting would destroy a key).
func TestPrimeRefusesLegacyUpgradeOverDifferentBytes(t *testing.T) {
	ctx := context.Background()
	st, kr, _ := setupKeyring(t, "a")
	if err := kr.resolve(ctx); err != nil {
		t.Fatal(err)
	}
	legacyWCK, err := dssync.NewWCK()
	if err != nil {
		t.Fatal(err)
	}
	if err := kr.KeyStore.StoreWCK(ctx, kr.workspaceID, 1, "", legacyWCK); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordKeyEpoch(ctx, 1, "", "legacy"); err != nil {
		t.Fatal(err)
	}
	// Corrupt the kid-aware target slot with different bytes before the upgrade.
	other, err := dssync.NewWCK()
	if err != nil {
		t.Fatal(err)
	}
	kid := dssync.KIDForWCK(legacyWCK)
	if err := kr.KeyStore.StoreWCK(ctx, kr.workspaceID, 1, kid, other); err != nil {
		t.Fatal(err)
	}

	if err := kr.Prime(ctx); err == nil {
		t.Fatal("Prime upgraded a legacy key over a mismatched kid-aware slot")
	}
	// The corrupted slot was not silently replaced.
	got, err := kr.KeyStore.LoadWCK(ctx, kr.workspaceID, 1, kid)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(other) {
		t.Fatal("Prime overwrote the existing kid-aware slot despite refusing")
	}
}

// Post-#56 Codex review (P1): Rotate must wrap every grant BEFORE writing any
// state. A malformed recipient on an approved device row must abort the
// rotation with NOTHING recorded — no new epoch row, no custody slot, no grant
// events — so the caller can never push under a half-minted epoch whose grants
// never published.
func TestRotateBadRecipientLeavesNoHalfMintedEpoch(t *testing.T) {
	ctx := context.Background()
	st, kr, _ := setupKeyring(t, "a")
	if _, err := kr.EnsureBootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertDevice(ctx, state.Device{
		ID: "dev_bad", Name: "bad", OS: "linux", Arch: "arm64",
		PublicKey: "not-an-age-recipient", TrustState: "approved",
	}); err != nil {
		t.Fatal(err)
	}
	pendingBefore, err := st.LocalPendingEvents(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := kr.Rotate(ctx); err == nil {
		t.Fatal("Rotate with a malformed approved recipient succeeded, want error")
	}
	epoch, err := st.CurrentKeyEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if epoch != 1 {
		t.Fatalf("CurrentKeyEpoch = %d after failed rotate, want 1 (no half-minted epoch row)", epoch)
	}
	keys, err := st.HeldKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if k.Epoch != 1 {
			t.Fatalf("held key at epoch %d after failed rotate, want epoch 1 only", k.Epoch)
		}
	}
	pendingAfter, err := st.LocalPendingEvents(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pendingAfter) != len(pendingBefore) {
		t.Fatalf("failed rotate queued %d new event(s), want none", len(pendingAfter)-len(pendingBefore))
	}
}
