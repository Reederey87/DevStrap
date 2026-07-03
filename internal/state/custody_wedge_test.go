package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/zalando/go-keyring"
)

// TestEnsureLocalEventSignatureRefusesDivergentMintOnUnreachableKeychain
// reproduces the exact P6-XP-04 wedge through the real event-stamping path: a
// device with an already-published signing key, then a run where the keychain
// is unreachable (a dead D-Bus session), must fail closed with a remedy — and
// must NOT drop a divergent signing-key file into the key dir the way the
// pre-fix code did.
func TestEnsureLocalEventSignatureRefusesDivergentMintOnUnreachableKeychain(t *testing.T) {
	// TestMain sets a reachable mock keyring; restore it for later tests.
	defer keyring.MockInit()

	ctx := context.Background()
	dir := t.TempDir()
	st, err := Open(ctx, filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(ctx, "personal", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	device, err := st.EnsureDevice(ctx, "laptop")
	if err != nil {
		t.Fatal(err)
	}

	// First event, keychain reachable: mints and publishes the signing key.
	if _, err := st.InsertLocalEvent(ctx, Event{Type: "project.added", PayloadJSON: `{"path":"work/a"}`}); err != nil {
		t.Fatalf("first InsertLocalEvent: %v", err)
	}
	dev, err := st.CurrentDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if dev.ID != device.ID || dev.SigningPublicKey == "" {
		t.Fatalf("first event did not publish a signing public key: %+v", dev)
	}

	// Now the keychain is unreachable (headless run, no session bus). go-keyring
	// surfaces an untyped godbus error; the platform seam classifies it.
	keyring.MockInitWithError(errors.New("dbus: DBUS_SESSION_BUS_ADDRESS not set and unable to locate session bus"))

	_, err = st.InsertLocalEvent(ctx, Event{Type: "project.updated", PayloadJSON: `{"path":"work/b"}`})
	if err == nil {
		t.Fatal("second InsertLocalEvent succeeded on an unreachable keychain, want the wedge refusal")
	}
	for _, want := range []string{"refusing to mint", "unreachable", "DEVSTRAP_NO_KEYCHAIN"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal error %q missing remedy fragment %q", err, want)
		}
	}

	// The wedge: no divergent signing-key file may appear in the key dir.
	entries, rerr := os.ReadDir(st.keyDir)
	if rerr != nil && !os.IsNotExist(rerr) {
		t.Fatalf("read key dir: %v", rerr)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".signing.key") {
			t.Fatalf("a divergent signing key file was written: %s", e.Name())
		}
	}
}

// TestKeyCustodyRoundTripIsWriteOnce: the custody decision is recorded once and
// honored thereafter — a later attempt to record a different backend does not
// overwrite it (P6-XP-04).
func TestKeyCustodyRoundTripIsWriteOnce(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	if got, err := st.KeyCustody(ctx); err != nil || got != devicekeys.CustodyUnset {
		t.Fatalf("initial KeyCustody = (%q, %v), want unset", got, err)
	}
	if err := st.RecordKeyCustody(ctx, devicekeys.CustodyKeychain); err != nil {
		t.Fatal(err)
	}
	// A second record with a different value must be ignored.
	if err := st.RecordKeyCustody(ctx, devicekeys.CustodyFile); err != nil {
		t.Fatal(err)
	}
	if got, err := st.KeyCustody(ctx); err != nil || got != devicekeys.CustodyKeychain {
		t.Fatalf("KeyCustody after re-record = (%q, %v), want keychain (write-once)", got, err)
	}
	// Unset is not a valid recorded decision.
	if err := st.RecordKeyCustody(ctx, devicekeys.CustodyUnset); err == nil {
		t.Fatal("RecordKeyCustody(unset) succeeded, want an error")
	}
}

// TestEffectiveKeyCustodyHonorsNoKeychainOverride: DEVSTRAP_NO_KEYCHAIN forces
// file custody regardless of the recorded decision (P6-XP-04).
func TestEffectiveKeyCustodyHonorsNoKeychainOverride(t *testing.T) {
	if got := EffectiveKeyCustody(devicekeys.CustodyKeychain); got != devicekeys.CustodyKeychain {
		t.Fatalf("without override = %q, want keychain", got)
	}
	t.Setenv("DEVSTRAP_NO_KEYCHAIN", "1")
	if got := EffectiveKeyCustody(devicekeys.CustodyKeychain); got != devicekeys.CustodyFile {
		t.Fatalf("with DEVSTRAP_NO_KEYCHAIN = %q, want file", got)
	}
}
