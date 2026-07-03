package devicekeys

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/platform"
)

// loadErrBackend fails Load with a fixed error but lets Store/Delete succeed and
// records values, so a mint can proceed after a not-found read.
type loadErrBackend struct {
	loadErr error
	values  map[string][]byte
}

func (b *loadErrBackend) Store(_ context.Context, service, account string, secret []byte) error {
	if b.values == nil {
		b.values = map[string][]byte{}
	}
	b.values[service+"/"+account] = append([]byte(nil), secret...)
	return nil
}

func (b *loadErrBackend) Load(_ context.Context, service, account string) ([]byte, error) {
	if v, ok := b.values[service+"/"+account]; ok {
		return append([]byte(nil), v...), nil
	}
	if b.loadErr != nil {
		return nil, b.loadErr
	}
	return nil, os.ErrNotExist
}

func (b *loadErrBackend) Delete(_ context.Context, service, account string) error { return nil }

// allErrBackend fails every operation with a fixed error.
type allErrBackend struct{ err error }

func (b allErrBackend) Store(context.Context, string, string, []byte) error { return b.err }
func (b allErrBackend) Load(context.Context, string, string) ([]byte, error) {
	return nil, b.err
}
func (b allErrBackend) Delete(context.Context, string, string) error { return b.err }

// TestEnsureSigningMintsOnTypedNotFound: a keychain that types its missing
// secret as platform.ErrSecretNotFound is treated as genuinely absent, so the
// mint proceeds and lands in the keychain (P6-XP-04 classification).
func TestEnsureSigningMintsOnTypedNotFound(t *testing.T) {
	dir := t.TempDir()
	backend := &loadErrBackend{loadErr: fmt.Errorf("%w: missing", platform.ErrSecretNotFound)}
	store := NewHybridStore(dir, backend).WithCustody(CustodyKeychain)

	id, created, err := store.EnsureSigning(t.Context(), "dev_test", "")
	if err != nil || !created {
		t.Fatalf("EnsureSigning = (%+v, created=%v, %v), want a fresh mint", id, created, err)
	}
	if _, ok := backend.values[keychainService+"/"+signingAccount("dev_test")]; !ok {
		t.Fatal("mint did not land in the keychain backend")
	}
	if _, err := os.Stat(filepath.Join(dir, "dev_test.signing.key")); !os.IsNotExist(err) {
		t.Fatalf("keychain-custody mint wrote a file fallback: %v", err)
	}
}

// TestEnsureSigningRefusesMintWhenUnreachableAndPublished: the exact P6-XP-04
// wedge in isolation — a device with an already-published signing key hitting an
// unreachable keychain must refuse to mint and must not write a key file.
func TestEnsureSigningRefusesMintWhenUnreachableAndPublished(t *testing.T) {
	dir := t.TempDir()
	backend := allErrBackend{err: fmt.Errorf("%w: session bus missing", platform.ErrUnsupported)}
	store := NewHybridStore(dir, backend) // unset custody

	_, created, err := store.EnsureSigning(t.Context(), "dev_test", "ed25519:cHVibGlzaGVk")
	if err == nil || created {
		t.Fatalf("EnsureSigning = (created=%v, %v), want a refusal", created, err)
	}
	if !errors.Is(err, ErrKeychainUnreachable) {
		t.Fatalf("refusal error = %v, want it to wrap ErrKeychainUnreachable", err)
	}
	for _, want := range []string{"key exists", "unreachable", platform.NoKeychainEnv, "desktop session"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal error %q missing remedy fragment %q", err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "dev_test.signing.key")); !os.IsNotExist(err) {
		t.Fatalf("refused mint still wrote a key file: %v", err)
	}
}

// TestEnsureSigningRefusesMintUnderKeychainCustody: even with nothing published,
// a store pinned to keychain custody must fail closed on an unreachable
// keychain rather than silently degrade to file custody.
func TestEnsureSigningRefusesMintUnderKeychainCustody(t *testing.T) {
	dir := t.TempDir()
	backend := allErrBackend{err: fmt.Errorf("%w: session bus missing", platform.ErrUnsupported)}
	store := NewHybridStore(dir, backend).WithCustody(CustodyKeychain)

	if _, _, err := store.EnsureSigning(t.Context(), "dev_test", ""); err == nil {
		t.Fatal("keychain-custody EnsureSigning succeeded on an unreachable backend, want fail closed")
	}
	if _, err := os.Stat(filepath.Join(dir, "dev_test.signing.key")); !os.IsNotExist(err) {
		t.Fatalf("keychain-custody refusal wrote a file fallback: %v", err)
	}
}

// TestEnsureSigningFileFallbackWhenUnreachableAndNothingPublished: the legacy
// headless path is preserved — under unset custody with no published key, an
// unreachable keychain falls back to the 0600 file store.
func TestEnsureSigningFileFallbackWhenUnreachableAndNothingPublished(t *testing.T) {
	dir := t.TempDir()
	backend := allErrBackend{err: fmt.Errorf("%w: session bus missing", platform.ErrUnsupported)}
	store := NewHybridStore(dir, backend) // unset custody, nothing published

	_, created, err := store.EnsureSigning(t.Context(), "dev_test", "")
	if err != nil || !created {
		t.Fatalf("EnsureSigning = (created=%v, %v), want a file-fallback mint", created, err)
	}
	info, err := os.Stat(filepath.Join(dir, "dev_test.signing.key"))
	if err != nil {
		t.Fatalf("file fallback not written: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("file fallback mode = %s, want 0600", got)
	}
}

// TestEnsureSigningFailsClosedOnUntypedError: devicekeys no longer string-matches
// keyring errors, so an untyped "dbus: connection refused" reaching the store
// (i.e. one the platform seam did not classify) is a hard failure — never
// treated as "unavailable" and never a silent file fallback.
func TestEnsureSigningFailsClosedOnUntypedError(t *testing.T) {
	dir := t.TempDir()
	backend := allErrBackend{err: errors.New("dbus: connection refused")}
	store := NewHybridStore(dir, backend) // unset custody, nothing published

	if _, _, err := store.EnsureSigning(t.Context(), "dev_test", ""); err == nil {
		t.Fatal("EnsureSigning succeeded on an untyped hard error, want fail closed")
	}
	if _, err := os.Stat(filepath.Join(dir, "dev_test.signing.key")); !os.IsNotExist(err) {
		t.Fatalf("untyped hard error wrote a file fallback: %v", err)
	}
}

// TestProbeClassifiesBackend: the init-time custody probe maps a nil or
// unreachable backend to file custody and a reachable (secret-absent) backend to
// keychain custody (P6-XP-04).
func TestProbeClassifiesBackend(t *testing.T) {
	if got := NewHybridStore(t.TempDir(), nil).Probe(t.Context()); got != CustodyFile {
		t.Fatalf("nil backend Probe = %q, want file", got)
	}
	unreachable := NewHybridStore(t.TempDir(), allErrBackend{err: fmt.Errorf("%w: no bus", platform.ErrUnsupported)})
	if got := unreachable.Probe(t.Context()); got != CustodyFile {
		t.Fatalf("unreachable backend Probe = %q, want file", got)
	}
	reachable := NewHybridStore(t.TempDir(), &loadErrBackend{loadErr: fmt.Errorf("%w: missing", platform.ErrSecretNotFound)})
	if got := reachable.Probe(t.Context()); got != CustodyKeychain {
		t.Fatalf("reachable backend Probe = %q, want keychain", got)
	}
}
