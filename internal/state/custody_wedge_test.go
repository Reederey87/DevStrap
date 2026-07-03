package state

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/platform"
)

// fakeKeychain is a hermetic, in-memory devicekeys.SecretBackend used to test
// custody behavior without touching the host keychain (which differs across CI
// runners). Unlike the go-keyring mock it preserves stored values across a
// reachable→unreachable toggle, so a wedge that depends on an already-stored key
// can be reproduced faithfully. When unreachable it returns a typed
// platform.ErrUnsupported, exactly as the platform seam classifies a missing
// Secret Service / session bus.
type fakeKeychain struct {
	mu          sync.Mutex
	values      map[string][]byte
	unreachable bool
}

func (k *fakeKeychain) setUnreachable(v bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.unreachable = v
}

func (k *fakeKeychain) Store(_ context.Context, service, account string, secret []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.unreachable {
		return fmt.Errorf("%w: fake session bus down", platform.ErrUnsupported)
	}
	if k.values == nil {
		k.values = map[string][]byte{}
	}
	k.values[service+"/"+account] = append([]byte(nil), secret...)
	return nil
}

func (k *fakeKeychain) Load(_ context.Context, service, account string) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.unreachable {
		return nil, fmt.Errorf("%w: fake session bus down", platform.ErrUnsupported)
	}
	if v, ok := k.values[service+"/"+account]; ok {
		return append([]byte(nil), v...), nil
	}
	return nil, os.ErrNotExist
}

func (k *fakeKeychain) Delete(_ context.Context, service, account string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.unreachable {
		return fmt.Errorf("%w: fake session bus down", platform.ErrUnsupported)
	}
	delete(k.values, service+"/"+account)
	return nil
}

// TestEnsureLocalEventSignatureRefusesDivergentMintOnUnreachableKeychain
// reproduces the exact P6-XP-04 wedge through the real event-stamping path: a
// device with an already-published signing key, then a run where the keychain
// is unreachable, must fail closed with a remedy — and must NOT drop a divergent
// signing-key file into the key dir the way the pre-fix code did. It injects a
// fake keychain through the package seam and controls DEVSTRAP_NO_KEYCHAIN
// explicitly, so it is hermetic on every host.
func TestEnsureLocalEventSignatureRefusesDivergentMintOnUnreachableKeychain(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "") // never inherit the CI job-level "1"
	fake := &fakeKeychain{}
	restore := swapKeychainBackend(func() devicekeys.SecretBackend { return fake })
	defer restore()

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

	// First event, keychain reachable: mints and publishes the signing key into
	// the fake keychain (not a file).
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

	// Now the keychain is unreachable (headless run, no session bus).
	fake.setUnreachable(true)

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
// file custody regardless of the recorded decision (P6-XP-04). It controls the
// env in both directions so it does not depend on the ambient CI job setting.
func TestEffectiveKeyCustodyHonorsNoKeychainOverride(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "")
	if got := EffectiveKeyCustody(devicekeys.CustodyKeychain); got != devicekeys.CustodyKeychain {
		t.Fatalf("without override = %q, want keychain", got)
	}
	t.Setenv(platform.NoKeychainEnv, "1")
	if got := EffectiveKeyCustody(devicekeys.CustodyKeychain); got != devicekeys.CustodyFile {
		t.Fatalf("with DEVSTRAP_NO_KEYCHAIN = %q, want file", got)
	}
}

// swapKeychainBackend replaces the package-level keychain seam and returns a
// restore func. Tests defer the restore so later tests see the default.
func swapKeychainBackend(fn func() devicekeys.SecretBackend) func() {
	old := keychainBackend
	keychainBackend = fn
	return func() { keychainBackend = old }
}
