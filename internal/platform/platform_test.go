package platform

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

func TestDetectReturnsCompleteSet(t *testing.T) {
	set := Detect()
	if set.OS == "" {
		t.Fatal("Detect OS is empty")
	}
	if set.Watcher == nil || set.Service == nil || set.Keychain == nil || set.Editor == nil {
		t.Fatalf("Detect returned incomplete set: %#v", set)
	}
	if (set.OS == "darwin" || set.OS == "linux") && set.Watcher.Name() != "fsnotify" {
		t.Fatalf("watcher = %q, want fsnotify for %s", set.Watcher.Name(), set.OS)
	}
}

func TestPollWatcherEmitsScanAndStopsOnContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	events := make(chan FSEvent, 1)
	errs := make(chan error, 1)
	go func() {
		errs <- PollWatcher{Interval: time.Hour}.Watch(ctx, "/tmp/work", events)
	}()

	select {
	case event := <-events:
		if event.Kind != FSEventScan || event.Path != "/tmp/work" || event.At.IsZero() {
			t.Fatalf("event = %#v, want scan for root", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for poll watcher event")
	}

	cancel()
	select {
	case err := <-errs:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("watch err = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for poll watcher shutdown")
	}
}

func TestUnsupportedAdaptersReturnSentinel(t *testing.T) {
	service := UnsupportedServiceManager{Target: "launchd"}
	if err := service.Install(t.Context(), ServiceSpec{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("service Install err = %v, want ErrUnsupported", err)
	}
	keychain := UnsupportedKeychain{Target: "keychain"}
	if _, err := keychain.Load(t.Context(), "svc", "acct"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("keychain Load err = %v, want ErrUnsupported", err)
	}
}

func TestSystemKeychainStoresLoadsAndDeletes(t *testing.T) {
	keyring.MockInit()
	keychain := SystemKeychain{Target: "mock-keychain"}
	if err := keychain.Store(t.Context(), "devstrap-test", "acct", []byte("secret")); err != nil {
		t.Fatal(err)
	}
	secret, err := keychain.Load(t.Context(), "devstrap-test", "acct")
	if err != nil {
		t.Fatal(err)
	}
	if string(secret) != "secret" {
		t.Fatalf("secret = %q, want secret", secret)
	}
	if err := keychain.Delete(t.Context(), "devstrap-test", "acct"); err != nil {
		t.Fatal(err)
	}
	if _, err := keychain.Load(t.Context(), "devstrap-test", "acct"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("Load after Delete err = %v, want ErrSecretNotFound", err)
	}
}
