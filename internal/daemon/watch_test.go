package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/platform"
)

// stubWatcher emits hints on demand and can be made to fail, so the degrade
// path is testable without exhausting real file descriptors.
type stubWatcher struct {
	name string
	// emit, when non-nil, is sent to the events channel each time it fires.
	emit chan time.Time
	// failWith, when set, makes Watch return immediately with this error.
	failWith error

	mu      sync.Mutex
	watched []string
}

func (s *stubWatcher) Name() string { return s.name }

func (s *stubWatcher) Watch(ctx context.Context, root string, events chan<- platform.FSEvent) error {
	s.mu.Lock()
	s.watched = append(s.watched, root)
	s.mu.Unlock()

	if s.failWith != nil {
		return s.failWith
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case at := <-s.emit:
			select {
			case events <- platform.FSEvent{Kind: platform.FSEventScan, Path: root, At: at}:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

func (s *stubWatcher) roots() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.watched...)
}

type stubSource struct {
	roots []string
	err   error
}

func (s stubSource) WatchRoots(context.Context) ([]string, error) { return s.roots, s.err }

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestWatcherTriggersNamespaceOnlyConvergence is the load-bearing invariant of
// the Milestone 5 entry gate, in code: a filesystem hint must NEVER cause
// materialization. An FSEvent cannot even name the file that changed, and this
// test pins that the trigger it produces is namespace-only — so no filesystem
// activity, accidental or hostile, can make DevStrap clone repositories.
func TestWatcherTriggersNamespaceOnlyConvergence(t *testing.T) {
	fake := newFakeConverger()
	close(fake.release)
	s := newScheduler(fake)
	stub := &stubWatcher{name: "stub", emit: make(chan time.Time, 1)}
	plane := newWatchPlane(stub, nil, stubSource{roots: []string{t.TempDir()}}, s, quietLogger())

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go plane.run(ctx)

	waitFor(t, func() bool { return len(stub.roots()) > 0 })
	stub.emit <- time.Now()
	waitFor(t, func() bool { return fake.count() > 0 })

	modes := fake.seenModes()
	for _, mode := range modes {
		if mode != TickNamespaceOnly {
			t.Fatalf("watcher-triggered cycle ran in mode %q; a filesystem hint must never materialize", mode)
		}
	}
}

// TestWatcherFloorsTriggerRate pins that a burst of hints cannot outpace
// convergence. The adapter's debounce bounds burst-to-hint; this floor bounds
// hint-to-convergence, which is the one that matters when a save-storm outlasts
// a cycle.
func TestWatcherFloorsTriggerRate(t *testing.T) {
	fake := newFakeConverger()
	close(fake.release)
	s := newScheduler(fake)
	stub := &stubWatcher{name: "stub", emit: make(chan time.Time)}
	plane := newWatchPlane(stub, nil, stubSource{roots: []string{t.TempDir()}}, s, quietLogger())

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go plane.run(ctx)
	waitFor(t, func() bool { return len(stub.roots()) > 0 })

	// Fire many hints back to back. Only the first may convert to a cycle,
	// because minTriggerInterval has not elapsed.
	for range 20 {
		select {
		case stub.emit <- time.Now():
		case <-time.After(2 * time.Second):
			t.Fatal("watcher stopped accepting hints")
		}
	}
	waitFor(t, func() bool { return plane.snapshot().Hints >= 20 })
	// Give any spurious extra trigger time to appear.
	time.Sleep(300 * time.Millisecond)

	if got := fake.count(); got != 1 {
		t.Fatalf("converger called %d times for 20 rapid hints, want 1 (minTriggerInterval floor)", got)
	}
}

// TestWatcherDegradesToPollingOnFailure covers PLAT-02: a native watcher that
// fails (EMFILE/ENOSPC on a large tree) must fall back to polling rather than
// silently losing the plane.
func TestWatcherDegradesToPollingOnFailure(t *testing.T) {
	fake := newFakeConverger()
	close(fake.release)
	s := newScheduler(fake)

	native := &stubWatcher{name: "fsnotify", failWith: errors.New("too many open files")}
	fallback := &stubWatcher{name: "poll", emit: make(chan time.Time, 1)}
	plane := newWatchPlane(native, fallback, stubSource{roots: []string{t.TempDir()}}, s, quietLogger())

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go plane.run(ctx)

	waitFor(t, func() bool {
		st := plane.snapshot()
		return st.Degraded && st.Backend == "poll"
	})
	st := plane.snapshot()
	if st.Reason == "" {
		t.Fatal("degradation recorded with no reason; a silent degrade leaves the user believing they have sub-interval convergence")
	}
	if len(fallback.roots()) == 0 {
		t.Fatal("fallback watcher was never started")
	}

	// The fallback must still produce namespace-only triggers.
	fallback.emit <- time.Now()
	waitFor(t, func() bool { return fake.count() > 0 })
	for _, mode := range fake.seenModes() {
		if mode != TickNamespaceOnly {
			t.Fatalf("degraded plane ran mode %q, want namespace-only", mode)
		}
	}
}

// TestWatcherReportsNoRootsWithoutFailing pins that a fresh workspace with
// nothing materialized is a normal state, not an error.
func TestWatcherReportsNoRootsWithoutFailing(t *testing.T) {
	s := newScheduler(newFakeConverger())
	plane := newWatchPlane(&stubWatcher{name: "stub"}, nil, stubSource{roots: nil}, s, quietLogger())

	done := make(chan struct{})
	go func() { defer close(done); plane.run(t.Context()) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return for an empty root set")
	}
	st := plane.snapshot()
	if !st.Degraded || st.Reason == "" {
		t.Fatalf("state = %+v, want a recorded reason for not watching", st)
	}
}

// TestWatcherSourceErrorDegradesRatherThanCrashing pins that the daemon
// survives an unreadable store — periodic convergence still runs.
func TestWatcherSourceErrorDegradesRatherThanCrashing(t *testing.T) {
	s := newScheduler(newFakeConverger())
	plane := newWatchPlane(&stubWatcher{name: "stub"}, nil, stubSource{err: errors.New("store locked")}, s, quietLogger())

	done := make(chan struct{})
	go func() { defer close(done); plane.run(t.Context()) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after a source error")
	}
	if st := plane.snapshot(); !st.Degraded {
		t.Fatalf("state = %+v, want degraded", st)
	}
}

// TestWatcherStopsOnCancel pins that the plane is bounded by its context.
func TestWatcherStopsOnCancel(t *testing.T) {
	s := newScheduler(newFakeConverger())
	stub := &stubWatcher{name: "stub", emit: make(chan time.Time)}
	plane := newWatchPlane(stub, nil, stubSource{roots: []string{t.TempDir(), t.TempDir()}}, s, quietLogger())

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); plane.run(ctx) }()
	waitFor(t, func() bool { return len(stub.roots()) == 2 })

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watch plane did not stop on cancel")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}
