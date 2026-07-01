package devicekeys

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type memorySecretBackend struct {
	values map[string][]byte
	err    error
}

func (b *memorySecretBackend) Store(_ context.Context, service, account string, secret []byte) error {
	if b.err != nil {
		return b.err
	}
	if b.values == nil {
		b.values = map[string][]byte{}
	}
	b.values[service+"/"+account] = append([]byte(nil), secret...)
	return nil
}

func (b *memorySecretBackend) Load(_ context.Context, service, account string) ([]byte, error) {
	if b.err != nil {
		return nil, b.err
	}
	value, ok := b.values[service+"/"+account]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), value...), nil
}

func (b *memorySecretBackend) Delete(_ context.Context, service, account string) error {
	if b.err != nil {
		return b.err
	}
	delete(b.values, service+"/"+account)
	return nil
}

func TestFileStoreEnsureCreatesAndReusesAgeIdentity(t *testing.T) {
	store := NewFileStore(t.TempDir())
	identity, created, err := store.Ensure("dev_test")
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("created = false, want true")
	}
	if identity.Private == "" || identity.Recipient == "" {
		t.Fatalf("identity = %+v, want private and recipient", identity)
	}
	if !strings.HasPrefix(identity.Private, "AGE-SECRET-KEY-1") {
		t.Fatalf("private key = %q, want AGE-SECRET-KEY-1 prefix", identity.Private)
	}
	if !strings.HasPrefix(identity.Recipient, "age1") {
		t.Fatalf("recipient = %q, want age1 prefix", identity.Recipient)
	}
	info, err := os.Stat(store.path("dev_test"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("identity permissions = %s, want 0600", got)
	}

	again, created, err := store.Ensure("dev_test")
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("second ensure created a new identity")
	}
	if again != identity {
		t.Fatalf("second identity = %+v, want %+v", again, identity)
	}
}

func TestFileStoreEnsureSigningCreatesAndReusesEd25519Identity(t *testing.T) {
	store := NewFileStore(t.TempDir())
	identity, created, err := store.EnsureSigning("dev_test")
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("created = false, want true")
	}
	if !strings.HasPrefix(identity.Private, "ed25519:") || !strings.HasPrefix(identity.Public, "ed25519:") {
		t.Fatalf("identity = %+v, want ed25519 keys", identity)
	}
	info, err := os.Stat(store.signingPath("dev_test"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("signing identity permissions = %s, want 0600", got)
	}
	signature, err := Sign(identity.Private, "devstrap.test", []byte("message"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(identity.Public, signature, "devstrap.test", []byte("message")); err != nil {
		t.Fatal(err)
	}
	if err := Verify(identity.Public, signature, "devstrap.test", []byte("tampered")); err == nil {
		t.Fatal("Verify accepted a tampered message")
	}

	again, created, err := store.EnsureSigning("dev_test")
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("second ensure created a new signing identity")
	}
	if again != identity {
		t.Fatalf("second signing identity = %+v, want %+v", again, identity)
	}
}

func TestHybridStorePrefersSecretBackend(t *testing.T) {
	dir := t.TempDir()
	backend := &memorySecretBackend{}
	store := NewHybridStore(dir, backend)
	identity, created, err := store.Ensure(t.Context(), "dev_test")
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("created = false, want true")
	}
	if _, err := os.Stat(filepath.Join(dir, "dev_test.agekey")); !os.IsNotExist(err) {
		t.Fatalf("file identity err = %v, want no file when keychain backend succeeds", err)
	}
	again, created, err := store.Ensure(t.Context(), "dev_test")
	if err != nil {
		t.Fatal(err)
	}
	if created || again != identity {
		t.Fatalf("second ensure = %+v created=%v, want existing %+v", again, created, identity)
	}

	signing, created, err := store.EnsureSigning(t.Context(), "dev_test")
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("signing created = false, want true")
	}
	signingAgain, created, err := store.EnsureSigning(t.Context(), "dev_test")
	if err != nil {
		t.Fatal(err)
	}
	if created || signingAgain != signing {
		t.Fatalf("second signing ensure = %+v created=%v, want existing %+v", signingAgain, created, signing)
	}
}

func TestHybridStoreFallsBackToFileStore(t *testing.T) {
	dir := t.TempDir()
	store := NewHybridStore(dir, &memorySecretBackend{err: errors.New("unsupported keychain")})
	identity, created, err := store.Ensure(t.Context(), "dev_test")
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("created = false, want true")
	}
	info, err := os.Stat(filepath.Join(dir, "dev_test.agekey"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("fallback identity permissions = %s, want 0600", got)
	}
	again, created, err := store.Ensure(t.Context(), "dev_test")
	if err != nil {
		t.Fatal(err)
	}
	if created || again != identity {
		t.Fatalf("second fallback ensure = %+v created=%v, want existing %+v", again, created, identity)
	}
}

func TestHybridStoreWCKRoundTrip(t *testing.T) {
	dir := t.TempDir()
	backend := &memorySecretBackend{}
	store := NewHybridStore(dir, backend)
	wck := []byte("0123456789abcdef0123456789abcdef") // 32 bytes
	const epoch = int64(1)

	if err := store.StoreWCK(t.Context(), "ws_test", epoch, wck); err != nil {
		t.Fatalf("StoreWCK: %v", err)
	}
	got, err := store.LoadWCK(t.Context(), "ws_test", epoch)
	if err != nil {
		t.Fatalf("LoadWCK: %v", err)
	}
	if string(got) != string(wck) {
		t.Fatalf("LoadWCK = %x, want %x", got, wck)
	}
}

func TestHybridStoreLoadWCKMissing(t *testing.T) {
	store := NewHybridStore(t.TempDir(), &memorySecretBackend{})
	if _, err := store.LoadWCK(t.Context(), "ws_test", 9); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadWCK missing: got %v, want os.ErrNotExist", err)
	}
}

func TestFileStoreWCKRoundTripAndPerms(t *testing.T) {
	// Exercise the file fallback store directly.
	store := NewFileStore(t.TempDir())
	wck := make([]byte, 32)
	for i := range wck {
		wck[i] = byte(i)
	}
	if err := store.WriteWCK("ws_test", 2, wck); err != nil {
		t.Fatalf("WriteWCK: %v", err)
	}
	got, err := store.ReadWCK("ws_test", 2)
	if err != nil {
		t.Fatalf("ReadWCK: %v", err)
	}
	if string(got) != string(wck) {
		t.Fatalf("ReadWCK = %x, want %x", got, wck)
	}
	info, err := os.Stat(store.wckPath("ws_test", 2))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("wck file permissions = %s, want 0600", got)
	}
}

func TestHybridStoreWCKFallsBackToFile(t *testing.T) {
	// A keychain backend that reports "unsupported" triggers the file fallback
	// (mirrors DEVSTRAP_NO_KEYCHAIN=1 -> platform.UnsupportedKeychain).
	backend := &memorySecretBackend{err: errors.New("keyring: unsupported platform")}
	store := NewHybridStore(t.TempDir(), backend)
	wck := []byte("0123456789abcdef0123456789abcdef")
	if err := store.StoreWCK(t.Context(), "ws_test", 3, wck); err != nil {
		t.Fatalf("StoreWCK file fallback: %v", err)
	}
	got, err := store.LoadWCK(t.Context(), "ws_test", 3)
	if err != nil {
		t.Fatalf("LoadWCK file fallback: %v", err)
	}
	if string(got) != string(wck) {
		t.Fatalf("LoadWCK = %x, want %x", got, wck)
	}
}

func TestStoreWCKRejectsInvalidWorkspaceID(t *testing.T) {
	store := NewHybridStore(t.TempDir(), &memorySecretBackend{})
	if err := store.StoreWCK(t.Context(), "ws/../escape", 1, make([]byte, 32)); err == nil {
		t.Fatal("StoreWCK with path-traversal workspace id unexpectedly succeeded")
	}
}
