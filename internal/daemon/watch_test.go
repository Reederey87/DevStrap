package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

type mutableSource struct {
	mu    sync.Mutex
	roots []string
	err   error
}

func (s *mutableSource) WatchRoots(context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.roots...), s.err
}

func (s *mutableSource) setRoots(roots []string) {
	s.mu.Lock()
	s.roots = append([]string(nil), roots...)
	s.mu.Unlock()
}

type controlledWatcher struct {
	name string

	mu       sync.Mutex
	fail     bool
	attempts int
}

func (w *controlledWatcher) Name() string { return w.name }

func (w *controlledWatcher) Watch(ctx context.Context, _ string, _ chan<- platform.FSEvent) error {
	w.mu.Lock()
	w.attempts++
	fail := w.fail
	w.mu.Unlock()
	if fail {
		return errors.New("temporary watcher failure")
	}
	<-ctx.Done()
	return ctx.Err()
}

// slowFailWatcher fails only after a delay, reproducing the motivating case:
// addRecursiveWatch walks thousands of directories before hitting the inotify
// or descriptor limit, so the failure arrives long after the arm began.
type slowFailWatcher struct {
	name  string
	delay time.Duration

	mu       sync.Mutex
	attempts int
}

type countingWatcher struct {
	mu     sync.Mutex
	counts map[string]int
	live   map[string]int
}

func (w *countingWatcher) Name() string { return "counting" }

func (w *countingWatcher) Watch(ctx context.Context, root string, _ chan<- platform.FSEvent) error {
	w.mu.Lock()
	if w.live == nil {
		w.live = make(map[string]int)
	}
	w.live[root] = w.counts[root]
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.live, root)
		w.mu.Unlock()
	}()
	<-ctx.Done()
	return ctx.Err()
}

func (w *countingWatcher) WatchedDirs() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	total := 0
	for _, count := range w.live {
		total += count
	}
	return total
}

func (w *slowFailWatcher) Name() string { return w.name }

func (w *slowFailWatcher) Watch(ctx context.Context, _ string, _ chan<- platform.FSEvent) error {
	w.mu.Lock()
	w.attempts++
	w.mu.Unlock()
	select {
	case <-time.After(w.delay):
		return errors.New("no space left on device")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *controlledWatcher) setFail(fail bool) {
	w.mu.Lock()
	w.fail = fail
	w.mu.Unlock()
}

func (w *controlledWatcher) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.attempts
}

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
	s := newScheduler(fake, nil)
	stub := &stubWatcher{name: "stub", emit: make(chan time.Time, 1)}
	plane := newWatchPlane(stub, nil, stubSource{roots: []string{t.TempDir()}}, s, quietLogger(), nil)

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
	s := newScheduler(fake, nil)
	stub := &stubWatcher{name: "stub", emit: make(chan time.Time)}
	plane := newWatchPlane(stub, nil, stubSource{roots: []string{t.TempDir()}}, s, quietLogger(), nil)

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
	s := newScheduler(fake, nil)

	native := &stubWatcher{name: "fsnotify", failWith: errors.New("too many open files")}
	fallback := &stubWatcher{name: "poll", emit: make(chan time.Time, 1)}
	plane := newWatchPlane(native, fallback, stubSource{roots: []string{t.TempDir()}}, s, quietLogger(), nil)

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

func TestWatchPhaseIsNeverEmptyOnTheWire(t *testing.T) {
	s := newScheduler(newFakeConverger(), nil)
	plane := newWatchPlane(&stubWatcher{name: "stub"}, nil, stubSource{roots: nil}, s, quietLogger(), nil)
	if got := plane.snapshot().Phase; got != watchPhaseStarting {
		t.Fatalf("phase before run = %q, want %q", got, watchPhaseStarting)
	}
	if plane.snapshot().Degraded {
		t.Fatal("a plane that has not started must not report degraded")
	}
}

// TestWatcherWithNoRootsIsIdleNotDegraded pins that a fresh workspace with
// nothing materialized is a normal live state, not an error.
// TestWatchPhaseIsNeverEmptyOnTheWire pins that the tri-state has no unnamed
// fourth member. The zero value of watchPhase is "", and reporting that on
// /v1/health would reintroduce the exact ambiguity this state field removes.
func TestWatcherWithNoRootsIsIdleNotDegraded(t *testing.T) {
	s := newScheduler(newFakeConverger(), nil)
	source := &mutableSource{}
	watcher := &stubWatcher{name: "stub", emit: make(chan time.Time)}
	plane := newWatchPlane(watcher, nil, source, s, quietLogger(), nil)
	plane.rediscoverInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); plane.run(ctx) }()
	waitFor(t, func() bool { return plane.snapshot().Phase == watchPhaseIdle })
	st := plane.snapshot()
	if st.Degraded || st.Reason != "no materialized projects yet" {
		t.Fatalf("state = %+v, want normal idle state", st)
	}
	select {
	case <-done:
		t.Fatal("watch plane returned while idle")
	default:
	}

	source.setRoots([]string{t.TempDir()})
	waitFor(t, func() bool { return plane.snapshot().Phase == watchPhaseWatching })
}

// TestWatcherSourceErrorDegradesRatherThanCrashing pins that the daemon
// survives an unreadable store — periodic convergence still runs.
func TestWatcherSourceErrorDegradesRatherThanCrashing(t *testing.T) {
	s := newScheduler(newFakeConverger(), nil)
	plane := newWatchPlane(&stubWatcher{name: "stub"}, nil, stubSource{err: errors.New("store locked")}, s, quietLogger(), nil)

	done := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	go func() { defer close(done); plane.run(ctx) }()
	waitFor(t, func() bool { return plane.snapshot().Phase == watchPhaseDegraded })
	if st := plane.snapshot(); !st.Degraded {
		t.Fatalf("state = %+v, want degraded", st)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watch plane did not stop after source-error cancellation")
	}
}

// TestWatcherStopsOnCancel pins that the plane is bounded by its context.
func TestWatcherStopsOnCancel(t *testing.T) {
	s := newScheduler(newFakeConverger(), nil)
	stub := &stubWatcher{name: "stub", emit: make(chan time.Time)}
	plane := newWatchPlane(stub, nil, stubSource{roots: []string{t.TempDir(), t.TempDir()}}, s, quietLogger(), nil)

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

func TestWatcherPicksUpProjectMaterializedAfterStart(t *testing.T) {
	s := newScheduler(newFakeConverger(), nil)
	source := &mutableSource{}
	watcher := &stubWatcher{name: "stub", emit: make(chan time.Time)}
	plane := newWatchPlane(watcher, nil, source, s, quietLogger(), nil)
	plane.rediscoverInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go plane.run(ctx)
	waitFor(t, func() bool { return plane.snapshot().Phase == watchPhaseIdle })

	source.setRoots([]string{t.TempDir()})
	waitFor(t, func() bool {
		st := plane.snapshot()
		return st.Phase == watchPhaseWatching && st.Roots == 1
	})
}

func TestWatcherDoesNotRearmWhenRootsUnchanged(t *testing.T) {
	s := newScheduler(newFakeConverger(), nil)
	watcher := &controlledWatcher{name: "native"}
	plane := newWatchPlane(watcher, nil, stubSource{roots: []string{t.TempDir()}}, s, quietLogger(), nil)
	plane.rediscoverInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go plane.run(ctx)
	waitFor(t, func() bool { return plane.snapshot().Phase == watchPhaseWatching })
	time.Sleep(80 * time.Millisecond)
	if got := watcher.count(); got != 1 {
		t.Fatalf("native watcher armed %d times for unchanged roots, want 1", got)
	}
}

func TestWatcherRetriesNativeAfterDegrade(t *testing.T) {
	s := newScheduler(newFakeConverger(), nil)
	native := &controlledWatcher{name: "native", fail: true}
	fallback := &stubWatcher{name: "poll", emit: make(chan time.Time)}
	plane := newWatchPlane(native, fallback, stubSource{roots: []string{t.TempDir()}}, s, quietLogger(), nil)
	plane.rediscoverInterval = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go plane.run(ctx)
	waitFor(t, func() bool { return plane.snapshot().Phase == watchPhaseDegraded })

	native.setFail(false)
	waitFor(t, func() bool {
		st := plane.snapshot()
		return st.Phase == watchPhaseWatching && !st.Degraded && st.Backend == "native"
	})
	if st := plane.snapshot(); st.Reason != "" {
		t.Fatalf("recovered state retained reason %q", st.Reason)
	}
}

// TestSlowFailingWatcherDoesNotFlapRecovery is the regression for the
// flagship failure mode. On a large tree the native watcher fails only after
// walking it, so a retry that declares "watching" on a short timer publishes a
// FALSE recovery and then a fresh degrade — once per retry, forever, producing
// exactly the alarm-flapping this plane exists to eliminate. A retry from
// degraded must survive a probation before it may claim recovery.
func TestSlowFailingWatcherDoesNotFlapRecovery(t *testing.T) {
	s := newScheduler(newFakeConverger(), nil)
	bus := newEventBus()
	_, events := bus.subscribe()
	// Models production proportions: the arm fails long after the short
	// healthy-path delay (10ms) but well within one re-discovery interval,
	// exactly as a large-tree walk fails in seconds against a 60s interval.
	// Under the previous code the 10ms timer declared "watching" first and
	// published a false recovery on every retry.
	native := &slowFailWatcher{name: "native", delay: 40 * time.Millisecond}
	plane := newWatchPlane(native, &stubWatcher{name: "poll"}, stubSource{roots: []string{t.TempDir()}}, s, quietLogger(), bus)
	plane.rediscoverInterval = 150 * time.Millisecond

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); plane.run(ctx) }()

	// Let several re-discovery ticks elapse so any per-retry flap would show.
	time.Sleep(400 * time.Millisecond)
	cancel()
	<-done

	var kinds []string
	for {
		select {
		case e := <-events:
			kinds = append(kinds, e.Detail)
			continue
		default:
		}
		break
	}

	recoveries := 0
	for _, d := range kinds {
		if strings.Contains(d, "recovered") {
			recoveries++
		}
	}
	if recoveries > 0 {
		t.Fatalf("published %d false recovery event(s) for a watcher that never recovered: %v", recoveries, kinds)
	}
	if plane.snapshot().Phase != watchPhaseDegraded {
		t.Fatalf("phase = %q, want %q for a persistently failing watcher", plane.snapshot().Phase, watchPhaseDegraded)
	}
}

// TestRecoveryProbationSurvivesTheRediscoveryTick pins the interaction between
// the two mechanisms added here, which cancelled each other. A probation lasts a
// full re-discovery interval, and a degraded plane re-arms on every tick — so if
// the loop treats its own probationary retry as just another degraded arm to
// replace, the tick tears it down and restarts the probation forever: descriptors
// rebuilt every cadence, recovery never announced, permanently degraded despite a
// perfectly healthy watcher.
//
// The interval here is deliberately shorter than the probation floor, so the tick
// ALWAYS lands mid-probation instead of racing it. That makes the failure
// deterministic rather than a coin flip.
func TestRecoveryProbationSurvivesTheRediscoveryTick(t *testing.T) {
	s := newScheduler(newFakeConverger(), nil)
	native := &controlledWatcher{name: "native", fail: true}
	plane := newWatchPlane(native, &stubWatcher{name: "poll"}, stubSource{roots: []string{t.TempDir()}}, s, quietLogger(), nil)
	// A 5ms cadence against the 10ms probation floor: every tick fires while
	// the retry is still on probation.
	plane.rediscoverInterval = 5 * time.Millisecond
	if plane.recoveryProbation() <= plane.rediscoverInterval {
		t.Fatalf("probation %v must exceed interval %v for this test to exercise the tick collision",
			plane.recoveryProbation(), plane.rediscoverInterval)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go plane.run(ctx)
	waitFor(t, func() bool { return plane.snapshot().Phase == watchPhaseDegraded })

	// The count at the moment the watcher turns healthy is the baseline: a
	// probation that survives recovers on the FIRST retry after this point,
	// while one that is cancelled every tick burns an arm per cadence and
	// recovers only if the loop happens to stall past the probation.
	native.setFail(false)
	before := native.count()
	waitFor(t, func() bool {
		st := plane.snapshot()
		return st.Phase == watchPhaseWatching && st.Backend == "native"
	})
	if spent := native.count() - before; spent > 3 {
		t.Fatalf("recovery consumed %d native arms, want at most a few: the probation is being restarted every tick", spent)
	}

	// And the recovered arm is then left alone rather than rebuilt every tick.
	settled := native.count()
	time.Sleep(60 * time.Millisecond)
	if got := native.count(); got != settled {
		t.Fatalf("native watcher re-armed %d more times after recovery, want a stable arm", got-settled)
	}
}

// TestNeedsRearmLeavesAProbationaryArmAlone pins the predicate itself, with no
// timing involved: it is the direct statement of the rule that the integration
// test above can only observe indirectly.
func TestNeedsRearmLeavesAProbationaryArmAlone(t *testing.T) {
	roots := []string{"/a", "/b"}
	newPlane := func(phase watchPhase, probation uint64) *watchPlane {
		p := newWatchPlane(nil, nil, stubSource{}, nil, quietLogger(), nil)
		p.state.Phase = phase
		p.probationArm = probation
		return p
	}

	cases := []struct {
		name      string
		phase     watchPhase
		probation uint64
		current   []string
		armAlive  bool
		want      bool
	}{
		{"degraded and polling re-arms to retry native", watchPhaseDegraded, 0, roots, true, true},
		{"degraded but on probation is left alone", watchPhaseDegraded, 7, roots, true, false},
		{"a dead arm always re-arms, probation or not", watchPhaseDegraded, 7, roots, false, true},
		{"changed roots always re-arm, probation or not", watchPhaseDegraded, 7, []string{"/a"}, true, true},
		{"a healthy watching arm is left alone", watchPhaseWatching, 0, roots, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newPlane(tc.phase, tc.probation)
			if got := p.needsRearm(tc.current, roots, tc.armAlive); got != tc.want {
				t.Fatalf("needsRearm = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWatchDegradedPublishesOncePerTransition(t *testing.T) {
	s := newScheduler(newFakeConverger(), nil)
	native := &controlledWatcher{name: "native", fail: true}
	fallback := &stubWatcher{name: "poll", emit: make(chan time.Time)}
	bus := newEventBus()
	id, events := bus.subscribe()
	defer bus.unsubscribe(id)
	plane := newWatchPlane(native, fallback, stubSource{roots: []string{t.TempDir()}}, s, quietLogger(), bus)
	plane.rediscoverInterval = 15 * time.Millisecond

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go plane.run(ctx)
	waitFor(t, func() bool { return native.count() >= 4 })
	if got := drainEvents(events); len(got) != 1 || got[0].Kind != EventWatchDegraded {
		t.Fatalf("repeated failures published events %+v, want one watch.degraded transition", got)
	}

	native.setFail(false)
	waitFor(t, func() bool { return plane.snapshot().Phase == watchPhaseWatching })
	got := waitForEvents(t, events, 1)
	if !strings.Contains(got[0].Detail, "recovered") {
		t.Fatalf("recovery event detail = %q, want recovery", got[0].Detail)
	}
	time.Sleep(50 * time.Millisecond)
	if extra := drainEvents(events); len(extra) != 0 {
		t.Fatalf("stable recovery published extra events: %+v", extra)
	}
}

func TestWatchHealthIdleIsVisibleOnTheWire(t *testing.T) {
	payload, err := json.Marshal(WatchHealth{
		Enabled:  true,
		State:    string(watchPhaseIdle),
		Degraded: false,
		Roots:    0,
		Reason:   "no materialized projects yet",
	})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"state", "degraded", "roots"} {
		if _, ok := fields[field]; !ok {
			t.Fatalf("%s absent from idle watch health JSON: %s", field, payload)
		}
	}
	if _, ok := fields["watched_dirs"]; ok {
		t.Fatalf("watched_dirs present for unknown idle count: %s", payload)
	}

	knownZero := WatchHealth{}
	field := reflect.ValueOf(&knownZero).Elem().FieldByName("WatchedDirs")
	switch field.Kind() {
	case reflect.Pointer:
		zero := 0
		field.Set(reflect.ValueOf(&zero))
	case reflect.Int:
		field.SetInt(0)
	default:
		t.Fatalf("WatchedDirs kind = %s, want pointer or int", field.Kind())
	}
	payload, err = json.Marshal(knownZero)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	if got, ok := fields["watched_dirs"]; !ok || got != float64(0) {
		t.Fatalf("watched_dirs = %#v, present = %v; want present 0 in %s", got, ok, payload)
	}
}

func TestHealthWatchBudgetOmittedWhenUnknown(t *testing.T) {
	server, err := New(Config{SocketPath: filepath.Join(t.TempDir(), "daemon.sock")})
	if err != nil {
		t.Fatal(err)
	}
	server.procRoot = filepath.Join(t.TempDir(), "missing-proc")
	recorder := httptest.NewRecorder()
	server.handleHealth(recorder, httptest.NewRequest(http.MethodGet, "/v1/health", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("health status = %d, want 200", recorder.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	watch, ok := payload["watch"].(map[string]any)
	if !ok {
		t.Fatalf("watch payload = %#v, want object", payload["watch"])
	}
	if _, ok := watch["watch_limit"]; ok {
		t.Fatalf("unknown watch_limit was emitted: %s", recorder.Body.String())
	}
	for key := range watch {
		if strings.Contains(strings.ToLower(key), "percent") || strings.Contains(strings.ToLower(key), "pct") {
			t.Fatalf("daemon emitted percentage field %q: %s", key, recorder.Body.String())
		}
	}
}

func TestWatchHealthReportsWatchedDirectoriesSummedAcrossRoots(t *testing.T) {
	roots := []string{t.TempDir(), t.TempDir()}
	if err := os.Mkdir(filepath.Join(roots[0], "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(roots[1], "child", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	watcher := &platform.NativeWatcher{}
	plane := newWatchPlane(watcher, nil, stubSource{}, nil, quietLogger(), nil)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- plane.watchAll(ctx, watcher, roots, make(chan platform.FSEvent)) }()
	waitFor(t, func() bool { return watcher.WatchedDirs() == 5 })
	plane.watching(watcher, len(roots))

	state := plane.snapshot()
	if !state.DirsKnown || state.WatchedDirs != 5 {
		t.Fatalf("snapshot dirs = %d, known = %v; want summed 5, true", state.WatchedDirs, state.DirsKnown)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("watchAll after cancellation: %v", err)
	}
}

func TestPollBackendReportsWatchedDirsAsUnknown(t *testing.T) {
	poll := platform.PollWatcher{Interval: time.Hour}
	plane := newWatchPlane(nil, poll, stubSource{}, nil, quietLogger(), nil)
	plane.setState(func(s *watchState) {
		s.Phase = watchPhaseDegraded
		s.Backend = poll.Name()
		s.Roots = 2
		s.counter, _ = any(poll).(platform.WatchedDirCounter)
	})
	state := plane.snapshot()
	if state.DirsKnown {
		t.Fatalf("poll directory count known as %d, want unknown", state.WatchedDirs)
	}
}

func TestDegradedPlaneDoesNotReportAStaleDirCount(t *testing.T) {
	watcher := &countingWatcher{}
	plane := newWatchPlane(watcher, nil, stubSource{}, nil, quietLogger(), nil)
	plane.watching(watcher, 1)
	plane.degrade("native failed")
	state := plane.snapshot()
	if state.DirsKnown || state.counter != nil {
		t.Fatalf("degraded state retained stale directory counter: %+v", state)
	}
}

func drainEvents(events <-chan Event) []Event {
	var got []Event
	for {
		select {
		case event := <-events:
			got = append(got, event)
		default:
			return got
		}
	}
}

func waitForEvents(t *testing.T, events <-chan Event, count int) []Event {
	t.Helper()
	got := make([]Event, 0, count)
	deadline := time.After(5 * time.Second)
	for len(got) < count {
		select {
		case event := <-events:
			got = append(got, event)
		case <-deadline:
			t.Fatalf("got %d events, want %d", len(got), count)
		}
	}
	return got
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

// blockingCounter lets a test park inside WatchedDirs() so a plane transition
// can be forced while the counter call is in flight — the exact window
// snapshot() re-validates against.
type blockingCounter struct {
	entered chan struct{}
	release chan struct{}
	dirs    int
}

func (c *blockingCounter) Name() string { return "blocking" }

func (c *blockingCounter) Watch(context.Context, string, chan<- platform.FSEvent) error {
	return nil
}

func (c *blockingCounter) WatchedDirs() int {
	close(c.entered)
	<-c.release
	return c.dirs
}

// TestSnapshotDoesNotPublishACountThePlaneNoLongerSupports pins the TOCTOU that
// the lock-release in snapshot() opens: the counter is read outside p.mu (it
// reaches into the platform adapter's own lock), so a concurrent degrade can
// land while the call is in flight. Publishing the returned count anyway would
// report watched_dirs beside a degraded state — precisely the stale-count
// confusion the field exists to avoid.
func TestSnapshotDoesNotPublishACountThePlaneNoLongerSupports(t *testing.T) {
	counter := &blockingCounter{entered: make(chan struct{}), release: make(chan struct{}), dirs: 5639}
	plane := newWatchPlane(counter, nil, stubSource{}, nil, quietLogger(), nil)
	plane.watching(counter, 1)

	done := make(chan watchState, 1)
	go func() { done <- plane.snapshot() }()

	<-counter.entered              // snapshot() is parked inside WatchedDirs()
	plane.degrade("native failed") // ...and the plane degrades underneath it
	close(counter.release)

	state := <-done
	if state.DirsKnown {
		t.Fatalf("published a directory count captured before a degrade: %+v", state)
	}
	if state.WatchedDirs != 0 {
		t.Fatalf("degraded snapshot carried WatchedDirs=%d, want 0", state.WatchedDirs)
	}
}
