package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/viper"
)

// TestFounderGateRequiresPullCursorZero pins the P6-SEC-02 founding proof: a
// keyless device whose pull cursor has already advanced past hub content (so a
// fresh pull legitimately returns rawSeen == 0) must NOT found the workspace —
// rawSeen == 0 alone proves "nothing new", not "hub empty". Without the
// pull-cursor check, a device that previously pulled events which all
// quarantined as permanent verification failures would self-mint a divergent
// epoch-1 key on a populated hub.
func TestFounderGateRequiresPullCursorZero(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	ctx := context.Background()
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	opts := &options{v: viper.New()}
	opts.v.Set("home", home)
	opts.v.Set("root", root)
	paths := opts.paths()
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}

	store, err := state.Open(ctx, paths.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureWorkspace(ctx, "keyless", root); err != nil {
		t.Fatal(err)
	}
	device, err := store.EnsureDevice(ctx, "keyless")
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureLocalDeviceIdentity(ctx, paths, store, device); err != nil {
		t.Fatal(err)
	}

	hubPath := filepath.Join(t.TempDir(), "hub.json")
	if err := os.WriteFile(hubPath, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	hub, hubID, err := hubFromOptions(ctx, opts, store, hubPath)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a prior cycle that observed hub content while keyless (e.g. all
	// pulled events quarantined as permanent verification failures, advancing
	// the safe cursor).
	if err := store.AdvanceHubCursor(ctx, hubID, 42); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	pushed, deferred, err := pushLocalEventsGated(ctx, &out, opts, store, hub, hubID, nil, 0)
	if err != nil {
		t.Fatalf("pushLocalEventsGated: %v", err)
	}
	if !deferred || pushed != 0 {
		t.Fatalf("pushed = %d, deferred = %v; want deferred with nothing pushed", pushed, deferred)
	}
	epoch, err := store.CurrentKeyEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if epoch != 0 {
		t.Fatalf("epoch after gated push = %d, want 0 (no self-mint on advanced pull cursor)", epoch)
	}

	// Control: against a hub this device has never observed (both cursors 0,
	// rawSeen == 0) the same device founds.
	freshHubID := hubID + "-fresh"
	pushed, deferred, err = pushLocalEventsGated(ctx, &out, opts, store, hub, freshHubID, nil, 0)
	if err != nil {
		t.Fatalf("pushLocalEventsGated (founding): %v", err)
	}
	if deferred || pushed != 0 {
		t.Fatalf("pushed = %d, deferred = %v; want founding push of zero events", pushed, deferred)
	}
	epoch, err = store.CurrentKeyEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if epoch != 1 {
		t.Fatalf("epoch after founding sync = %d, want 1", epoch)
	}
}

// TestFounderGateChecksPerDeviceCursors (P5-SYNC-01): the founding proof must
// consult the NEW hub_device_cursors table too — a keyless device whose only
// trace of prior hub interaction is a per-device seq cursor row (the legacy
// hub_cursors rows are frozen at 0 for post-migration devices) must not
// self-found on rawSeen == 0.
func TestFounderGateChecksPerDeviceCursors(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	ctx := context.Background()
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	opts := &options{v: viper.New()}
	opts.v.Set("home", home)
	opts.v.Set("root", root)
	paths := opts.paths()
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}

	store, err := state.Open(ctx, paths.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureWorkspace(ctx, "keyless", root); err != nil {
		t.Fatal(err)
	}
	device, err := store.EnsureDevice(ctx, "keyless")
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureLocalDeviceIdentity(ctx, paths, store, device); err != nil {
		t.Fatal(err)
	}

	hubPath := filepath.Join(t.TempDir(), "hub.json")
	if err := os.WriteFile(hubPath, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	hub, hubID, err := hubFromOptions(ctx, opts, store, hubPath)
	if err != nil {
		t.Fatal(err)
	}
	// A prior cycle consumed another device's stream through seq 3; the legacy
	// hub_cursors rows stay untouched (post-migration device).
	if err := store.AdvanceHubDeviceCursor(ctx, hubID, "dev_other", 3); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	pushed, deferred, err := pushLocalEventsGated(ctx, &out, opts, store, hub, hubID, nil, 0)
	if err != nil {
		t.Fatalf("pushLocalEventsGated: %v", err)
	}
	if !deferred || pushed != 0 {
		t.Fatalf("pushed = %d, deferred = %v; want deferred with nothing pushed", pushed, deferred)
	}
	epoch, err := store.CurrentKeyEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if epoch != 0 {
		t.Fatalf("epoch = %d, want 0 (no self-mint when a per-device cursor row exists)", epoch)
	}
}
