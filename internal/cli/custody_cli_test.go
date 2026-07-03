package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/config"
	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/zalando/go-keyring"
)

// TestInitRecordsFileCustodyUnderNoKeychain: with DEVSTRAP_NO_KEYCHAIN=1 the
// init probe records file custody, later runs honor it, and doctor reports it
// (P6-XP-04).
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
	// The row names file custody (with a NO_KEYCHAIN-override note).
	if !strings.Contains(stdout, "file") && !strings.Contains(stdout, "forcing file") {
		t.Fatalf("doctor stdout = %q, want the custody row to report file custody", stdout)
	}
}

// TestRecordKeyCustodyAtInitNeverStrandsAnAlreadyInitializedStore is the P2-1
// regression: a pre-00016 store whose device secrets live only in the keychain,
// run headless for the first time after upgrade (keychain unreachable), must NOT
// have `file` custody recorded from the negative probe — that would permanently
// route later desktop runs away from the keychain. Custody stays unset (legacy
// hybrid + the mint guard), and a subsequent desktop run reads the keychain
// secret fine.
func TestRecordKeyCustodyAtInitNeverStrandsAnAlreadyInitializedStore(t *testing.T) {
	defer keyring.MockInit() // restore the reachable mock for later tests

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
	keyring.MockInitWithError(errors.New("dbus: DBUS_SESSION_BUS_ADDRESS not set and unable to locate session bus"))
	if err := recordKeyCustodyAtInit(ctx, paths, st, device); err != nil {
		t.Fatalf("recordKeyCustodyAtInit: %v", err)
	}
	if got, _ := st.KeyCustody(ctx); got != devicekeys.CustodyUnset {
		t.Fatalf("KeyCustody after headless init = %q, want it to STAY unset (no file recorded)", got)
	}
	// No file-store key file may have been written on the headless run either.
	if entries, rerr := os.ReadDir(paths.KeyDir()); rerr == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".agekey") || strings.HasSuffix(e.Name(), ".signing.key") {
				t.Fatalf("headless run wrote a file-store key %q; the store must stay keychain-bound", e.Name())
			}
		}
	}

	// The store the next (desktop) run resolves is NOT pinned to file custody:
	// it stays unset, so it consults the keychain where the real secrets live —
	// the anti-stranding guarantee. (The mock keyring drops its store on each
	// MockInit swap, so a literal read-after-restore cannot be exercised here;
	// on a real keychain the persisted secret is read normally.)
	desktop, err := resolveKeyStore(ctx, paths, st)
	if err != nil {
		t.Fatal(err)
	}
	if desktop.Custody == devicekeys.CustodyFile {
		t.Fatal("resolved store is pinned to file custody after a headless run — the store was stranded away from the keychain")
	}
}

// TestRecordedFileCustodyNeverConsultsKeychain is the P2-2 regression: a store
// with recorded `file` custody reads the file-store key and never lets a
// (stale) keychain entry at the same account shadow it — the run path threads
// this custody through resolveKeyStore.
func TestRecordedFileCustodyNeverConsultsKeychain(t *testing.T) {
	defer keyring.MockInit()

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
	// A DIFFERENT key sits in the (reachable) keychain at the same account — the
	// stale entry that must not win.
	keychainStore := devicekeys.NewHybridStore(paths.KeyDir(), platform.Detect().Keychain).
		WithCustody(devicekeys.CustodyKeychain)
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
