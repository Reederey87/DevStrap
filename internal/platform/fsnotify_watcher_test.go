package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/ignore"
	"github.com/fsnotify/fsnotify"
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
func startWatcher(ctx context.Context, w *NativeWatcher, root string, buffer int) (chan FSEvent, chan error) {
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

func TestNativeWatcherReportsWatchedDirectoryCount(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{
		"kept",
		filepath.Join("kept", "nested"),
		filepath.Join("node_modules", "pkg", "nested"),
	} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "kept", "file.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	watcher := &NativeWatcher{}
	ctx, cancel := context.WithCancel(t.Context())
	_, errs := startWatcher(ctx, watcher, root, 1)

	// root, kept, and kept/nested are watched. node_modules and all files are
	// excluded, pinning that WatchList counts un-pruned directories.
	const want = 3
	deadline := time.Now().Add(10 * time.Second)
	for watcher.WatchedDirs() != want && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := watcher.WatchedDirs(); got != want {
		t.Fatalf("WatchedDirs() = %d, want %d un-pruned directories", got, want)
	}

	cancel()
	assertContextCanceled(t, errs)
	if got := watcher.WatchedDirs(); got != 0 {
		t.Fatalf("WatchedDirs() after Watch returned = %d, want 0", got)
	}
	watcher.mu.Lock()
	live := len(watcher.live)
	watcher.mu.Unlock()
	if live != 0 {
		t.Fatalf("live watcher registry after Watch returned = %d, want 0", live)
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
	watcher := &NativeWatcher{Debounce: 30 * time.Millisecond, MaxLatency: 150 * time.Millisecond}
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
			watcher := &NativeWatcher{Debounce: 30 * time.Millisecond, MaxLatency: 150 * time.Millisecond}
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

func TestWatcherPrunesCompilerIgnoredDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".devstrapignore"), []byte("generated/\n"), 0o600); err != nil {
		t.Fatalf("write .devstrapignore: %v", err)
	}
	for _, dir := range []string{".venv", "dist", "__pycache__", "generated", "src"} {
		if err := os.MkdirAll(filepath.Join(root, dir, "deep"), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	watcher := &NativeWatcher{Debounce: 30 * time.Millisecond, MaxLatency: 150 * time.Millisecond}
	events, errs := startWatcher(ctx, watcher, root, 1024)
	awaitWatcherReady(t, root, events)

	for _, dir := range []string{".venv", "dist", "__pycache__", "generated"} {
		path := filepath.Join(root, dir, "deep", "ignored.txt")
		if err := os.WriteFile(path, []byte("ignored"), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	if hints := drain(events, 700*time.Millisecond); hints != 0 {
		t.Fatalf("ignored-directory writes produced %d hints, want 0", hints)
	}

	if err := os.WriteFile(filepath.Join(root, "src", "deep", "real.go"), []byte("package real"), 0o600); err != nil {
		t.Fatalf("write real source: %v", err)
	}
	select {
	case <-events:
	case <-time.After(3 * time.Second):
		t.Fatal("watcher did not report a write in a real source directory")
	}

	cancel()
	assertContextCanceled(t, errs)
}

func TestWatcherPrunesDirectoryRenamedIntoPlace(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "build-tmp")
	deep := filepath.Join(source, "deep")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	watcher := &NativeWatcher{Debounce: 30 * time.Millisecond, MaxLatency: 150 * time.Millisecond}
	events, errs := startWatcher(ctx, watcher, root, 1024)
	awaitWatcherReady(t, root, events)

	target := filepath.Join(root, "build")
	if err := os.Rename(source, target); err != nil {
		t.Fatalf("rename into ignored path: %v", err)
	}
	drain(events, 700*time.Millisecond)

	if err := os.WriteFile(filepath.Join(target, "deep", "ignored.txt"), []byte("ignored"), 0o600); err != nil {
		t.Fatalf("write in renamed ignored directory: %v", err)
	}
	if hints := drain(events, 700*time.Millisecond); hints != 0 {
		t.Fatalf("write in directory renamed into ignored place produced %d hints, want 0", hints)
	}

	if err := os.WriteFile(filepath.Join(root, "real.txt"), []byte("real"), 0o600); err != nil {
		t.Fatalf("write real file: %v", err)
	}
	select {
	case <-events:
	case <-time.After(3 * time.Second):
		t.Fatal("watcher stopped after a directory was renamed into an ignored path")
	}

	cancel()
	assertContextCanceled(t, errs)
}

// TestWatcherDropsWatchesForDirectoryRenamedOutOfTree covers the descriptor leak
// PLAT-01 named: before this, nothing in internal/platform ever called
// watcher.Remove, so the watch set only grew for the life of a Watch call and a
// long-lived supervised daemon marched toward EMFILE. Note it is the DESCENDANT
// watch that leaks — the kernel drops the renamed directory's own watch, but
// everything below it survives.
func TestWatcherDropsWatchesForDirectoryRenamedOutOfTree(t *testing.T) {
	root := t.TempDir()
	removed := filepath.Join(root, "removed", "nested")
	if err := os.MkdirAll(removed, 0o755); err != nil {
		t.Fatal(err)
	}

	raw, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	events := make(chan FSEvent, 1024)
	errs := make(chan error, 1)
	watcher := &NativeWatcher{Debounce: 30 * time.Millisecond, MaxLatency: 150 * time.Millisecond}
	go func() {
		errs <- watcher.watch(ctx, root, events, raw, ignore.DefaultMatcher())
	}()
	awaitWatcherReady(t, root, events)

	if got := len(raw.WatchList()); got != 3 {
		t.Fatalf("initial watch count = %d, want 3 (%v)", got, raw.WatchList())
	}
	// Rename OUT of the tree rather than delete. Both inotify and kqueue drop a
	// watch when the watched inode is unlinked, so a deletion test passes with or
	// without the explicit Remove and proves nothing about it. A rename-out
	// leaves the inode very much alive outside the tree — precisely the
	// descriptor this adapter used to hold for the life of the Watch call.
	outside := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(filepath.Join(root, "removed"), outside); err != nil {
		t.Fatalf("rename watched directory out of the tree: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for len(raw.WatchList()) != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := raw.WatchList(); len(got) != 1 {
		t.Fatalf("watch count after rename-out = %d, want root only (%v)", len(got), got)
	}

	cancel()
	assertContextCanceled(t, errs)
}

func TestWatcherIgnoresChmodOnlyEvents(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "watched.txt")
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	watcher := &NativeWatcher{Debounce: 30 * time.Millisecond, MaxLatency: 150 * time.Millisecond}
	events, errs := startWatcher(ctx, watcher, root, 1024)
	awaitWatcherReady(t, root, events)

	for i := range 10 {
		mode := os.FileMode(0o600)
		if i%2 == 0 {
			mode = 0o640
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatalf("chmod burst: %v", err)
		}
	}
	if hints := drain(events, 700*time.Millisecond); hints != 0 {
		t.Fatalf("chmod-only burst produced %d hints, want 0", hints)
	}

	if err := os.WriteFile(path, []byte("after"), 0o600); err != nil {
		t.Fatalf("write watched file: %v", err)
	}
	select {
	case <-events:
	case <-time.After(3 * time.Second):
		t.Fatal("watcher did not report a write after ignoring chmod events")
	}

	cancel()
	assertContextCanceled(t, errs)
}

// TestWatcherIgnoresOSJunkFiles pins that junk filtering is UNCONDITIONAL —
// hence the negating .devstrapignore. Routing this through the compiled matcher
// instead would let a user pattern re-admit the noise, because defaults are
// applied before user patterns. Wanting `foo~` to be SYNCED (a legitimate
// content choice, which this ignore file expresses) is not the same as wanting a
// convergence hint every time an editor writes a backup.
func TestWatcherIgnoresOSJunkFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".devstrapignore"),
		[]byte("!*~\n!.DS_Store\n!4913\n"), 0o600); err != nil {
		t.Fatalf("write .devstrapignore: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	watcher := &NativeWatcher{Debounce: 30 * time.Millisecond, MaxLatency: 150 * time.Millisecond}
	events, errs := startWatcher(ctx, watcher, root, 1024)
	awaitWatcherReady(t, root, events)

	for _, name := range []string{".DS_Store", "4913", "editor.txt~"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("junk"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if hints := drain(events, 700*time.Millisecond); hints != 0 {
		t.Fatalf("OS/editor junk produced %d hints, want 0", hints)
	}

	if err := os.WriteFile(filepath.Join(root, "real.txt"), []byte("real"), 0o600); err != nil {
		t.Fatalf("write real file: %v", err)
	}
	select {
	case <-events:
	case <-time.After(3 * time.Second):
		t.Fatal("watcher did not report a real file after ignoring junk")
	}

	cancel()
	assertContextCanceled(t, errs)
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
	watcher := &NativeWatcher{Debounce: 30 * time.Millisecond, MaxLatency: 150 * time.Millisecond}

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
	watcher := &NativeWatcher{Debounce: 20 * time.Millisecond, MaxLatency: 100 * time.Millisecond}
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
	watcher := &NativeWatcher{Debounce: 20 * time.Millisecond, MaxLatency: 100 * time.Millisecond}
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

// TestWatchSetIsExactlyTheCompilerSurvivors is the real PLAT-01 pin: it asserts
// the WATCH SET, not hint counts.
//
// Asserting hints cannot detect the defect PLAT-01 names. The file-event filter
// suppresses hints from inside an ignored directory even when that directory is
// still watched, so restoring the old hardcoded `.git/node_modules/.devstrap/
// vendor` list leaves a hint-based test green while every `.venv`, `dist`,
// `target` and user-ignored tree quietly consumes descriptors again — which is
// the entire finding.
func TestWatchSetIsExactlyTheCompilerSurvivors(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".devstrapignore"), []byte("generated/\n"), 0o600); err != nil {
		t.Fatalf("write .devstrapignore: %v", err)
	}
	// One directory per exclusion source: canonical default, user pattern,
	// watcher-local, junk-named — plus real source that must survive.
	for _, dir := range []string{
		filepath.Join(".venv", "deep"),
		filepath.Join("dist", "deep"),
		filepath.Join("target", "deep"),
		filepath.Join("generated", "deep"),
		filepath.Join("vendor", "deep"),
		filepath.Join(".Trash", "deep"),
		filepath.Join("backup~", "deep"),
		filepath.Join("src", "deep"),
	} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	raw, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	matcher, err := ignore.CompileFromDir(root, true)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if err := addRecursiveWatch(raw, root, root, matcher); err != nil {
		t.Fatalf("addRecursiveWatch: %v", err)
	}

	want := map[string]bool{
		root:                               true,
		filepath.Join(root, "src"):         true,
		filepath.Join(root, "src", "deep"): true,
	}
	got := map[string]bool{}
	for _, w := range raw.WatchList() {
		got[w] = true
	}
	if len(got) != len(want) {
		t.Fatalf("watch set = %v, want exactly %v", raw.WatchList(), want)
	}
	for path := range want {
		if !got[path] {
			t.Fatalf("watch set = %v, missing %s", raw.WatchList(), path)
		}
	}
}

// TestWatcherSurvivesTransientDirectories covers the failure that made every
// other benefit in this change unobservable: a directory created and deleted
// before the walk reaches it used to return ENOENT, which is terminal for the
// watcher. Any `go test`, `npm`, or build run inside a watched project does
// exactly that, so the native watcher died within seconds of arming and re-died
// on every 60s retry — permanently degraded on any machine actually in use.
func TestWatcherSurvivesTransientDirectories(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	watcher := &NativeWatcher{Debounce: 20 * time.Millisecond, MaxLatency: 100 * time.Millisecond}
	events, errs := startWatcher(ctx, watcher, root, 4096)
	awaitWatcherReady(t, root, events)

	for i := range 12 {
		dir := filepath.Join(root, fmt.Sprintf("transient-%d", i), "sub")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.RemoveAll(filepath.Join(root, fmt.Sprintf("transient-%d", i))); err != nil {
			t.Fatalf("remove transient dir: %v", err)
		}
		select {
		case err := <-errs:
			t.Fatalf("watcher died after %d transient directories: %v", i+1, err)
		case <-time.After(60 * time.Millisecond):
		}
	}

	// Still alive and still reporting real changes.
	drain(events, 300*time.Millisecond)
	if err := os.WriteFile(filepath.Join(root, "real.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write real file: %v", err)
	}
	select {
	case <-events:
	case err := <-errs:
		t.Fatalf("DIAG watcher exited: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("watcher stopped reporting after transient directory churn")
	}

	cancel()
	assertContextCanceled(t, errs)
}

// TestWatcherReportsDeletionOfFileNamedLikeAPrunedDirectory pins the unknown-path
// reading. A deleted path cannot be stat'd, and the two readings disagree: `build`
// is pruned as a directory but is ordinary content as a file. Forcing the
// directory reading swallowed these deletions entirely while still reporting
// WRITES to the same file — silence in the direction that loses data.
func TestWatcherReportsDeletionOfFileNamedLikeAPrunedDirectory(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "build")
	if err := os.WriteFile(path, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	watcher := &NativeWatcher{Debounce: 20 * time.Millisecond, MaxLatency: 100 * time.Millisecond}
	events, errs := startWatcher(ctx, watcher, root, 1024)
	awaitWatcherReady(t, root, events)

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove %s: %v", path, err)
	}
	select {
	case <-events:
	case <-time.After(3 * time.Second):
		t.Fatal("deleting a file named like a pruned directory produced no hint")
	}

	cancel()
	assertContextCanceled(t, errs)
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

// TestEventPathUnderRoot pins the guard against the backend's own bad output.
// fsnotify's kqueue backend emits events with an empty or "." Name after a watch
// is removed. A relative name resolves against the process working directory, so
// without this guard the create branch would recursively watch the daemon's cwd —
// which is how the descriptor-release change first showed up as a lost-hint bug.
func TestEventPathUnderRoot(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "workspace", "code")
	cases := []struct {
		name string
		want bool
	}{
		{filepath.Join(root, "project"), true},
		{filepath.Join(root, "a", "b", "c"), true},
		{root, true},
		{"", false},              // observed on darwin after Remove
		{".", false},             // observed on darwin after Remove
		{"relative/path", false}, // resolves against cwd
		{filepath.Join(string(filepath.Separator), "elsewhere"), false},
		{filepath.Join(root, "..", "sibling"), false},
		{root + "-sibling", false}, // shared prefix, different tree
	}
	for _, tc := range cases {
		if got := eventPathUnderRoot(root, tc.name); got != tc.want {
			t.Errorf("eventPathUnderRoot(%q, %q) = %v, want %v", root, tc.name, got, tc.want)
		}
	}
}

// TestNativeWatcherSurvivesUnreadableDirectory pins P9-DAEMON-01's lower half.
// An unreadable directory yields a permission error from filepath.WalkDir that
// is NOT fs.ErrNotExist, and the walk previously returned it as a hard error —
// which propagated out of Watch, and whose caller's shared cancel then tore down
// every other root's watch, permanently. A local, recoverable condition (a test
// fixture exercising EACCES, a root-owned bind-mount) must not abandon the tree:
// the subtree is skipped and the periodic cycle covers it, exactly as for a
// directory that vanished.
func TestNativeWatcherSurvivesUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission denial is not reproducible")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "ok", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(root, "restricted")
	if err := os.MkdirAll(filepath.Join(bad, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bad, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(bad, 0o755) })

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Skipf("fsnotify unavailable: %v", err)
	}
	defer watcher.Close()

	if err := addRecursiveWatch(watcher, root, root, nil); err != nil {
		t.Fatalf("an unreadable directory must be skipped, not fail the whole walk: %v", err)
	}
	// The readable siblings must still be watched — skipping must not mean
	// abandoning the tree.
	var sawOK bool
	for _, w := range watcher.WatchList() {
		if strings.Contains(w, string(filepath.Separator)+"ok") {
			sawOK = true
		}
	}
	if !sawOK {
		t.Fatalf("readable siblings must still be watched after skipping the unreadable subtree; got %v", watcher.WatchList())
	}
}
