package workspacekeys

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
)

type memSecret struct{ values map[string][]byte }

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
	t.Helper()
	ctx := context.Background()
	st, err := state.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
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
	return st, New(st, keyStore), identity
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
	w2, _ := kr.cached(2)
	w1, _ := kr.cached(1)
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
	fw2, _ := fresh.cached(2)
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
