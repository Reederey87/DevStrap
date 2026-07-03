package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Reederey87/DevStrap/internal/config"
	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/state"
)

// fakeKeychain is a hermetic, in-memory devicekeys.SecretBackend used to test
// custody behavior without touching the host keychain (which differs across CI
// runners: dead session bus on Linux, interaction-not-allowed on macOS). It
// preserves stored values across a reachable→unreachable toggle, and when
// unreachable returns a typed platform.ErrUnsupported exactly as the platform
// seam classifies a missing Secret Service.
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

// swapKeychainBackend replaces the package-level keychain seam and returns a
// restore func so a test can inject a fake and clean up deterministically.
func swapKeychainBackend(fn func() devicekeys.SecretBackend) func() {
	old := keychainBackend
	keychainBackend = fn
	return func() { keychainBackend = old }
}

// TestInitRecordsFileCustodyUnderNoKeychain: with DEVSTRAP_NO_KEYCHAIN=1 the
// init probe records file custody, later runs honor it, and doctor reports it
// (P6-XP-04). The env is set explicitly so the test is deterministic on any host.
func TestInitRecordsFileCustodyUnderNoKeychain(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--workspace-name", "personal"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}

	st, err := state.Open(context.Background(), filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	custody, err := st.KeyCustody(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if custody != devicekeys.CustodyFile {
		t.Fatalf("recorded custody = %q, want file", custody)
	}

	stdout, stderr, err := executeForTest("--home", home, "doctor")
	if err != nil {
		t.Fatalf("doctor stderr = %q err = %v", stderr, err)
	}
	if !strings.Contains(stdout, "key custody") {
		t.Fatalf("doctor stdout = %q, want a key custody row", stdout)
	}
	if !strings.Contains(stdout, "file") && !strings.Contains(stdout, "forcing file") {
		t.Fatalf("doctor stdout = %q, want the custody row to report file custody", stdout)
	}
}

// TestRecordKeyCustodyAtInitNeverStrandsAnAlreadyInitializedStore is the P2-1
// regression: a pre-00016 store whose device secrets live only in the keychain,
// run headless for the first time after upgrade (keychain unreachable), must NOT
// have `file` custody recorded from the negative probe — that would permanently
// route later desktop runs away from the keychain. Custody stays unset (legacy
// hybrid + the mint guard), and the resolved store is never pinned to file. A
// fake keychain is injected and the env controlled explicitly, so it is hermetic.
func TestRecordKeyCustodyAtInitNeverStrandsAnAlreadyInitializedStore(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "") // never inherit the CI job-level "1"
	fake := &fakeKeychain{}
	restore := swapKeychainBackend(func() devicekeys.SecretBackend { return fake })
	defer restore()

	ctx := context.Background()
	home := t.TempDir()
	paths := config.Paths{Home: home, Root: t.TempDir()}
	st, err := state.Open(ctx, paths.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(ctx, "personal", paths.Root); err != nil {
		t.Fatal(err)
	}
	device, err := st.EnsureDevice(ctx, "laptop")
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a pre-00016 store: keys provisioned into the (reachable) keychain
	// and published, but NO custody decision recorded.
	keyStore, err := resolveKeyStore(ctx, paths, st)
	if err != nil {
		t.Fatal(err)
	}
	ageID, _, err := keyStore.Ensure(ctx, device.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetDevicePublicKey(ctx, device.ID, ageID.Recipient); err != nil {
		t.Fatal(err)
	}
	signID, _, err := keyStore.EnsureSigning(ctx, device.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeviceSigningPublicKey(ctx, device.ID, signID.Public); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.KeyCustody(ctx); got != devicekeys.CustodyUnset {
		t.Fatalf("precondition: KeyCustody = %q, want unset (pre-00016)", got)
	}
	device, err = st.CurrentDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// First headless run after upgrade: keychain unreachable.
	fake.setUnreachable(true)
	if err := recordKeyCustodyAtInit(ctx, paths, st, device); err != nil {
		t.Fatalf("recordKeyCustodyAtInit: %v", err)
	}
	if got, _ := st.KeyCustody(ctx); got != devicekeys.CustodyUnset {
		t.Fatalf("KeyCustody after headless init = %q, want it to STAY unset (no file recorded)", got)
	}
	// No file-store key file may have been written on the headless run either
	// (the keys live in the fake keychain, not on disk).
	if entries, rerr := os.ReadDir(paths.KeyDir()); rerr == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".agekey") || strings.HasSuffix(e.Name(), ".signing.key") {
				t.Fatalf("headless run wrote a file-store key %q; the store must stay keychain-bound", e.Name())
			}
		}
	}

	// The store the next (desktop) run resolves is NOT pinned to file custody:
	// it stays unset and keeps consulting the keychain where the real secrets
	// live — the anti-stranding guarantee. With the keychain reachable again the
	// published secret reads back.
	fake.setUnreachable(false)
	desktop, err := resolveKeyStore(ctx, paths, st)
	if err != nil {
		t.Fatal(err)
	}
	if desktop.Custody == devicekeys.CustodyFile {
		t.Fatal("resolved store is pinned to file custody after a headless run — the store was stranded")
	}
	got, err := desktop.Read(ctx, device.ID)
	if err != nil {
		t.Fatalf("desktop Read of keychain secret: %v", err)
	}
	if got.Recipient != device.PublicKey {
		t.Fatalf("desktop Read recipient = %q, want the published keychain key %q", got.Recipient, device.PublicKey)
	}
}

// TestRecordedFileCustodyNeverConsultsKeychain is the P2-2 regression: a store
// with recorded `file` custody reads the file-store key and never lets a
// (stale) keychain entry at the same account shadow it. A reachable fake
// keychain is injected and the env cleared, so it is hermetic.
func TestRecordedFileCustodyNeverConsultsKeychain(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "")
	fake := &fakeKeychain{}
	restore := swapKeychainBackend(func() devicekeys.SecretBackend { return fake })
	defer restore()

	ctx := context.Background()
	home := t.TempDir()
	paths := config.Paths{Home: home, Root: t.TempDir()}
	st, err := state.Open(ctx, paths.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	const deviceID = "dev_filecustody"
	// A DIFFERENT key sits in the (reachable) fake keychain at the same account —
	// the stale entry that must not win.
	keychainStore := devicekeys.NewHybridStore(paths.KeyDir(), fake).WithCustody(devicekeys.CustodyKeychain)
	keychainID, _, err := keychainStore.Ensure(ctx, deviceID, "")
	if err != nil {
		t.Fatal(err)
	}
	// The authoritative key lives in the file store.
	fileID, _, err := devicekeys.NewFileStore(paths.KeyDir()).Ensure(deviceID)
	if err != nil {
		t.Fatal(err)
	}
	if fileID.Recipient == keychainID.Recipient {
		t.Fatal("test setup: file and keychain keys collided; cannot prove custody routing")
	}

	if err := st.RecordKeyCustody(ctx, devicekeys.CustodyFile); err != nil {
		t.Fatal(err)
	}
	keyStore, err := resolveKeyStore(ctx, paths, st)
	if err != nil {
		t.Fatal(err)
	}
	got, err := keyStore.Read(ctx, deviceID)
	if err != nil {
		t.Fatalf("Read under file custody: %v", err)
	}
	if got.Recipient != fileID.Recipient {
		t.Fatalf("Read recipient = %q, want the file key %q (keychain must not shadow it)", got.Recipient, fileID.Recipient)
	}
	if got.Recipient == keychainID.Recipient {
		t.Fatal("file-custody Read returned the stale keychain key")
	}
}
