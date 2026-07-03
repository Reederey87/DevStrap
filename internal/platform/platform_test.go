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

// TestMapKeyringErrorClassification (P6-XP-04): the platform seam is the single
// place that turns go-keyring's error vocabulary into the typed sentinels the
// key-custody layer relies on. Crucially, an untyped godbus "session bus
// missing" error — which go-keyring does NOT surface as ErrUnsupportedPlatform —
// must map to ErrUnsupported here so higher layers never string-match it.
func TestMapKeyringErrorClassification(t *testing.T) {
	cases := []struct {
		name string
		in   error
		want error
	}{
		{"unsupported platform", keyring.ErrUnsupportedPlatform, ErrUnsupported},
		{"not found", keyring.ErrNotFound, ErrSecretNotFound},
		{"dbus session bus missing", errors.New("dbus: DBUS_SESSION_BUS_ADDRESS not set and unable to locate session bus"), ErrUnsupported},
		{"dbus connection refused", errors.New("dial unix /run/user/1000/bus: connect: connection refused"), ErrUnsupported},
		{"secret service not provided", errors.New("dbus: org.freedesktop.secrets was not provided by any .service files"), ErrUnsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mapKeyringError(tc.in, "test")
			if !errors.Is(got, tc.want) {
				t.Fatalf("mapKeyringError(%v) = %v, want it to wrap %v", tc.in, got, tc.want)
			}
			if !errors.Is(got, tc.in) {
				t.Fatalf("mapKeyringError(%v) dropped the underlying error: %v", tc.in, got)
			}
		})
	}

	// A live-backend hard failure stays untyped so custody fails closed rather
	// than treating it as unavailable.
	hard := mapKeyringError(errors.New("keychain io failure: device busy"), "test")
	if errors.Is(hard, ErrUnsupported) || errors.Is(hard, ErrSecretNotFound) {
		t.Fatalf("hard failure %v was misclassified as a typed sentinel", hard)
	}
}

// TestSecretServiceUnreachableRejectsLiveServiceErrors (CodeRabbit, PR #62):
// errors a LIVE Secret Service can produce — timeouts, dismissed prompts,
// generic dbus-prefixed failures — must NOT classify as backend-unavailable;
// only the missing-bus/missing-service signatures may.
func TestSecretServiceUnreachableRejectsLiveServiceErrors(t *testing.T) {
	live := []string{
		"dbus: operation timed out",
		"org.freedesktop.secrets: prompt dismissed",
		"dbus call failed: org.freedesktop.DBus.Error.NoReply",
	}
	for _, msg := range live {
		if secretServiceUnreachable(errors.New(msg)) {
			t.Errorf("live-service error %q misclassified as unreachable", msg)
		}
	}
	dead := []string{
		"dbus: couldn't determine address of the session bus",
		"dial unix /run/user/1000/bus: connect: connection refused",
		"The name org.freedesktop.secrets was not provided by any .service files",
	}
	for _, msg := range dead {
		if !secretServiceUnreachable(errors.New(msg)) {
			t.Errorf("dead-bus error %q not classified as unreachable", msg)
		}
	}
}
