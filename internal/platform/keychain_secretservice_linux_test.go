//go:build linux

package platform

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
)

// secretServiceEnv is the env var the CI job sets to declare that a real
// freedesktop Secret Service is up on the session bus. It is deliberately an
// explicit opt-in rather than a probe: a probe that silently decides "no
// keyring here" turns this whole file into a no-op, which is exactly the state
// it exists to end.
const secretServiceEnv = "DEVSTRAP_TEST_SECRET_SERVICE"

func requireSecretService(t *testing.T) {
	t.Helper()
	if os.Getenv(secretServiceEnv) != "1" {
		t.Skipf("%s != 1; run under the Secret Service CI job (dbus-run-session + gnome-keyring)", secretServiceEnv)
	}
	// Setting both is contradictory and almost certainly a misconfigured job:
	// DEVSTRAP_NO_KEYCHAIN=1 is the operator's explicit "this host has no
	// usable keychain", while the opt-in above asserts a live Secret Service.
	//
	// Precisely: these tests construct SystemKeychain DIRECTLY, so they bypass
	// newSet and NO_KEYCHAIN would NOT actually swap in UnsupportedKeychain
	// here — the round trip below would still reach the real backend. The
	// refusal is therefore conservative rather than load-bearing, and it fails
	// rather than skips so a contradictory job surfaces as a red build instead
	// of silently reporting coverage it does not have.
	if os.Getenv(NoKeychainEnv) == "1" {
		t.Fatalf("%s=1 and %s=1 are contradictory: the first declares this host has no usable "+
			"keychain, the second asserts a live Secret Service; refusing rather than guessing "+
			"which the job meant", NoKeychainEnv, secretServiceEnv)
	}
}

func linuxSecretService() SystemKeychain {
	return SystemKeychain{Platform: "linux", Target: "secret-service"}
}

// TestSecretServiceRoundTrip is the first execution this backend has ever had.
//
// Until now CI set DEVSTRAP_NO_KEYCHAIN=1 in every job, routing all Linux runs
// to UnsupportedKeychain — so the code path that decides where a Linux user's
// DEVICE PRIVATE KEYS live had never run. `devicekeys` fails closed to file
// custody when the keychain is unreachable and records that decision once at
// init (P6-XP-04), which makes an untested reachable-path a custody decision
// nobody has observed.
func TestSecretServiceRoundTrip(t *testing.T) {
	requireSecretService(t)
	k := linuxSecretService()
	ctx := context.Background()

	const service = "devstrap.test.secret-service"
	account := "roundtrip"
	secret := []byte("a-secret-with-\x00-and-utf8-Ω")

	t.Cleanup(func() { _ = k.Delete(ctx, service, account) })

	if err := k.Store(ctx, service, account, secret); err != nil {
		t.Fatalf("Store against a live Secret Service: %v", err)
	}
	got, err := k.Load(ctx, service, account)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !bytes.Equal(got, secret) {
		t.Fatalf("Load returned %q, want %q — a lossy round trip here silently corrupts a device key", got, secret)
	}
	if err := k.Delete(ctx, service, account); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := k.Load(ctx, service, account); err == nil {
		t.Fatal("Load succeeded after Delete; the secret was not actually removed")
	}
}

// TestSecretServiceMissingSecretIsNotUnreachable pins the distinction the
// custody decision turns on. `devicekeys.Probe` treats "backend reachable, key
// absent" as keychain custody and "backend unreachable" as file custody. If a
// missing secret were classified as unreachable, a healthy keychain device
// would silently migrate to the file store.
func TestSecretServiceMissingSecretIsNotUnreachable(t *testing.T) {
	requireSecretService(t)
	k := linuxSecretService()

	_, err := k.Load(context.Background(), "devstrap.test.secret-service", "definitely-absent")
	if err == nil {
		t.Fatal("expected an error loading an absent secret")
	}
	if errors.Is(err, ErrUnsupported) {
		t.Fatalf("an ABSENT secret was classified as an unreachable backend (%v); "+
			"that flips a healthy keychain device to file custody", err)
	}
}
