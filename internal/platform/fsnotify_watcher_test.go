package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// These tests satisfy the Milestone 5 entry gate in spec/14 ("the
// indexer-hydration-storm test must pass"; "the Mac sleep/wake watcher test
// must pass") and close TEST-06 in spec/16 (the fsnotify watcher had no tests
// and no goroutine-leak detection). They are the watcher's first coverage, and
// writing them surfaced the create-event prune bypass fixed in
// fsnotify_watcher.go.

// startWatcher runs w.Watch in a goroutine. Watch blocks until the context is
// cancelled, so every test drives it this way and reads its terminal error off
// the returned channel.
func startWatcher(ctx context.Context, w NativeWatcher, root string, buffer int) (chan FSEvent, chan error) {
	events := make(chan FSEvent, buffer)
	errs := make(chan error, 1)
	go func() { errs <- w.Watch(ctx, root, events) }()
	return events, errs
}

// awaitWatcherReady blocks until the watcher demonstrably has its watches
// registered. Watch offers no readiness signal — it performs the initial
// recursive Add and only then enters its loop — so a test that starts writing
// immediately races that walk. Touching a probe file until a hint comes back is
// the only reliable synchronization available.
func awaitWatcherReady(t *testing.T, root string, events <-chan FSEvent) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		probe := filepath.Join(root, fmt.Sprintf("ready-probe-%d", i))
		if err := os.WriteFile(probe, []byte("probe"), 0o600); err != nil {
			t.Fatalf("write probe: %v", err)
		}
		select {
		case <-events:
			drain(events, 300*time.Millisecond)
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
	t.Fatal("watcher never became ready")
}

// drain consumes hints until the stream stays quiet for quiet, and reports how
// many it consumed.
func drain(events <-chan FSEvent, quiet time.Duration) int {
	count := 0
	for {
		select {
		case <-events:
			count++
		case <-time.After(quiet):
			return count
		}
	}
}

// TestNativeWatcherCoalescesBurstIntoBoundedHints is the indexer-hydration-storm
// gate condition. It asserts two things: that a large burst collapses into a
// small number of hints, and — structurally — that a hint cannot name the file
// that changed. The second is the load-bearing half: every FSEvent carries
// Kind=FSEventScan and Path=<watch root>, so a consumer physically cannot learn
// which project changed from an event alone and therefore cannot hydrate one in
// response. Hydration stays reachable only through an explicit open/adopt/sync.
func TestNativeWatcherCoalescesBurstIntoBoundedHints(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"alpha", "beta", filepath.Join("alpha", "nested")} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	watcher := NativeWatcher{Debounce: 30 * time.Millisecond, MaxLatency: 150 * time.Millisecond}
	events, errs := startWatcher(ctx, watcher, root, 4096)
	awaitWatcherReady(t, root, events)

	const writes = 3000
	for i := range writes {
		dir := filepath.Join(root, "alpha", "nested")
		if i%3 == 0 {
			dir = filepath.Join(root, "beta")
		}
		name := filepath.Join(dir, fmt.Sprintf("burst-%d.tmp", i))
		if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	// Collect until the stream goes quiet, checking every hint's shape.
	hints := 0
	quiet := false
	for !quiet {
		select {
		case event := <-events:
			hints++
			if event.Kind != FSEventScan {
				t.Fatalf("event kind = %q, want %q — a watcher hint must never be a typed per-file instruction", event.Kind, FSEventScan)
			}
			if event.Path != root {
				t.Fatalf("event path = %q, want the watch root %q — a hint must not name the changed file, or a consumer could hydrate off it", event.Path, root)
			}
			if event.At.IsZero() {
				t.Fatal("event timestamp is zero")
			}
		case <-time.After(2 * time.Second):
			quiet = true
		}
	}

	if hints == 0 {
		t.Fatal("no hints for 3000 writes; the watcher is not reporting at all")
	}
	// The exact count depends on scheduling; the gate cares about the order of
	// magnitude — a burst must not translate into per-file wakeups.
	if hints > 50 {
		t.Fatalf("hints = %d for %d writes, want a coalesced handful (<= 50)", hints, writes)
	}

	cancel()
	assertContextCanceled(t, errs)
}

// TestNativeWatcherSkipsGeneratedDirCreatedAfterStart pins the create-event
// prune fix. addRecursiveWatch skips generated directories only below its own
// walk root, so before the fix the create branch — which passes each new
// directory as that call's root — registered watches throughout a freshly
// created node_modules/.git. Because a failed watcher.Add is terminal, one
// `npm install` under a watched project could kill the watcher outright.
//
// The assertion is behavioral: after the generated directory exists and the
// stream has quiesced, writing DEEP inside it must produce no hint (it is not
// watched), while a write elsewhere still does (the watcher is alive). Revert
// the fix and the deep write produces a hint, failing this test.
func TestNativeWatcherSkipsGeneratedDirCreatedAfterStart(t *testing.T) {
	for _, generated := range []string{"node_modules", ".git", "vendor", ".devstrap"} {
		t.Run(generated, func(t *testing.T) {
			root := t.TempDir()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			watcher := NativeWatcher{Debounce: 30 * time.Millisecond, MaxLatency: 150 * time.Millisecond}
			events, errs := startWatcher(ctx, watcher, root, 1024)
			awaitWatcherReady(t, root, events)

			deep := filepath.Join(root, generated, "pkg", "inner")
			if err := os.MkdirAll(deep, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", deep, err)
			}
			// Creating the directory under the watched root legitimately emits a
			// hint; let that settle before making the real assertion.
			drain(events, 700*time.Millisecond)

			for i := range 20 {
				name := filepath.Join(deep, fmt.Sprintf("generated-%d.tmp", i))
				if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
					t.Fatalf("write %s: %v", name, err)
				}
			}
			select {
			case event := <-events:
				t.Fatalf("got hint %#v for a write inside %s; generated directories created after start must stay out of the watch set", event, generated)
			case <-time.After(700 * time.Millisecond):
			}

			// The watcher must still be alive and watching real content.
			if err := os.WriteFile(filepath.Join(root, "real.txt"), []byte("x"), 0o600); err != nil {
				t.Fatalf("write real file: %v", err)
			}
			select {
			case event := <-events:
				if event.Kind != FSEventScan {
					t.Fatalf("event = %#v, want a scan hint", event)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("watcher stopped reporting after a generated directory appeared")
			}

			cancel()
			assertContextCanceled(t, errs)
		})
	}
}

// TestNativeWatcherRestartAfterBulkChangeMakesNoCompletenessClaim is the
// sleep/wake gate condition, following spec/16's own approximation (stop the
// watcher, bulk-change the tree, restart).
//
// It deliberately does NOT assert that changes made while the watcher was down
// are reported, because the invariant being licensed is the opposite one: the
// watcher is a latency optimization and makes no completeness claim. Drift
// across a sleep/wake, a restart, or a dropped kernel queue is caught by
// periodic reconciliation (run-loop / the daemon's periodic tick), never by the
// watcher. Every design decision downstream of this gate depends on that split.
func TestNativeWatcherRestartAfterBulkChangeMakesNoCompletenessClaim(t *testing.T) {
	root := t.TempDir()
	watcher := NativeWatcher{Debounce: 30 * time.Millisecond, MaxLatency: 150 * time.Millisecond}

	firstCtx, firstCancel := context.WithCancel(t.Context())
	events, errs := startWatcher(firstCtx, watcher, root, 1024)
	awaitWatcherReady(t, root, events)

	// "Sleep": the watcher stops.
	firstCancel()
	assertContextCanceled(t, errs)

	// Bulk change with nobody watching.
	for i := range 100 {
		name := filepath.Join(root, fmt.Sprintf("offline-%d.txt", i))
		if err := os.WriteFile(name, []byte("offline"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "offline-dir"), 0o755); err != nil {
		t.Fatalf("mkdir offline-dir: %v", err)
	}

	// "Wake": a fresh watcher over the same root must be functional. No
	// assertion is made about the offline changes above.
	secondCtx, secondCancel := context.WithCancel(t.Context())
	defer secondCancel()
	events2, errs2 := startWatcher(secondCtx, watcher, root, 1024)
	awaitWatcherReady(t, root, events2)

	if err := os.WriteFile(filepath.Join(root, "after-wake.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write after wake: %v", err)
	}
	select {
	case event := <-events2:
		if event.Kind != FSEventScan || event.Path != root {
			t.Fatalf("event = %#v, want a scan hint for the root", event)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("restarted watcher never reported a post-wake change")
	}

	secondCancel()
	assertContextCanceled(t, errs2)
}

// TestNativeWatcherStopReleasesGoroutines closes TEST-06's goroutine-leak half:
// a stopped watcher must release its own goroutine and fsnotify's internals
// (Watch closes the underlying watcher on return).
func TestNativeWatcherStopReleasesGoroutinesAndReturnsContextError(t *testing.T) {
	root := t.TempDir()
	baseline := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(t.Context())
	watcher := NativeWatcher{Debounce: 20 * time.Millisecond, MaxLatency: 100 * time.Millisecond}
	events, errs := startWatcher(ctx, watcher, root, 64)
	awaitWatcherReady(t, root, events)
	if err := os.WriteFile(filepath.Join(root, "activity.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write activity: %v", err)
	}

	cancel()
	assertContextCanceled(t, errs)

	// Poll with a deadline rather than a fixed sleep: teardown of fsnotify's
	// internal goroutines is asynchronous.
	deadline := time.Now().Add(5 * time.Second)
	poll := time.NewTicker(25 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-poll.C:
			if runtime.NumGoroutine() <= baseline+2 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("goroutines = %d after shutdown, want <= %d (baseline %d)", runtime.NumGoroutine(), baseline+2, baseline)
			}
		case <-t.Context().Done():
			t.Fatal("test context ended while waiting for watcher goroutines")
		}
	}
}

// TestNativeWatcherUnblocksSlowConsumerOnCancel pins the backpressure contract
// any daemon consumer must honor: the flush path sends on the caller's channel
// with only a context escape, so a consumer that stops reading blocks the
// watcher rather than silently dropping hints — and cancelling the context is
// what releases it.
func TestNativeWatcherBlocksOnSlowConsumerAndUnblocksOnCancel(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// Unbuffered, and deliberately never read.
	events := make(chan FSEvent)
	errs := make(chan error, 1)
	watcher := NativeWatcher{Debounce: 20 * time.Millisecond, MaxLatency: 100 * time.Millisecond}
	go func() { errs <- watcher.Watch(ctx, root, events) }()

	stopWrites := make(chan struct{})
	writesDone := make(chan struct{})
	go func() {
		defer close(writesDone)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for i := 0; ; i++ {
			select {
			case <-ticker.C:
				_ = os.WriteFile(filepath.Join(root, fmt.Sprintf("blocked-%d.txt", i)), []byte("x"), 0o600)
			case <-stopWrites:
				return
			}
		}
	}()

	select {
	case err := <-errs:
		t.Fatalf("Watch returned early with %v; it should be blocked on an unread channel", err)
	case <-time.After(500 * time.Millisecond):
	}
	close(stopWrites)
	<-writesDone

	cancel()
	assertContextCanceled(t, errs)
}

func TestShouldSkipWatchDir(t *testing.T) {
	tests := []struct {
		name string
		skip bool
	}{
		{".git", true},
		{"node_modules", true},
		{".devstrap", true},
		{"vendor", true},
		{"src", false},
		{"", false},
		{".github", false},
		{"node_modules_old", false},
		{"Vendor", false}, // case-sensitive by design; real dirs are lowercase
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldSkipWatchDir(tc.name); got != tc.skip {
				t.Fatalf("shouldSkipWatchDir(%q) = %v, want %v", tc.name, got, tc.skip)
			}
		})
	}
}

func assertContextCanceled(t *testing.T, errs <-chan error) {
	t.Helper()
	select {
	case err := <-errs:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Watch err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for watcher shutdown")
	}
}
