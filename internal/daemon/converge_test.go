package daemon

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeConverger records calls and blocks until released, so a test can hold a
// cycle open and observe what concurrent callers do.
type fakeConverger struct {
	mu       sync.Mutex
	calls    int
	modes    []TickMode
	release  chan struct{}
	err      error
	started  chan struct{}
	startOne sync.Once
}

func newFakeConverger() *fakeConverger {
	return &fakeConverger{release: make(chan struct{}), started: make(chan struct{})}
}

func (f *fakeConverger) Converge(ctx context.Context, mode TickMode) (Result, error) {
	f.mu.Lock()
	f.calls++
	f.modes = append(f.modes, mode)
	f.mu.Unlock()
	f.startOne.Do(func() { close(f.started) })

	select {
	case <-f.release:
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	return Result{}, f.err
}

func (f *fakeConverger) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeConverger) seenModes() []TickMode {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]TickMode(nil), f.modes...)
}

// TestSchedulerCoalescesConcurrentTriggers is the core contract: a burst of
// triggers must produce ONE convergence, not one per trigger. Without this a
// watcher hint storm would queue a cycle per event.
func TestSchedulerCoalescesConcurrentTriggers(t *testing.T) {
	fake := newFakeConverger()
	s := newScheduler(fake)

	const callers = 16
	var wg sync.WaitGroup
	var coalesced atomic.Int32
	results := make(chan Result, callers)

	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := s.Converge(t.Context(), TickFull)
			if err != nil {
				t.Errorf("Converge: %v", err)
				return
			}
			if result.Coalesced {
				coalesced.Add(1)
			}
			results <- result
		}()
	}

	// Let the first caller enter the converger, then release everyone.
	<-fake.started
	// Give the rest a moment to pile up behind the in-flight cycle. There is no
	// observable signal for "all callers have blocked", so this wait is
	// deliberate rather than a stand-in for synchronization.
	time.Sleep(200 * time.Millisecond)
	close(fake.release)
	wg.Wait()
	close(results)

	if got := fake.count(); got != 1 {
		t.Fatalf("converger called %d times, want exactly 1 — concurrent triggers must coalesce", got)
	}
	if got := int(coalesced.Load()); got != callers-1 {
		t.Fatalf("%d callers reported Coalesced, want %d (every joiner but the one that started the cycle)", got, callers-1)
	}
	if len(results) != callers {
		t.Fatalf("got %d results, want %d — every caller must get an answer", len(results), callers)
	}
}

// TestSchedulerRunsSequentialTriggersSeparately is the other half: coalescing
// must not swallow a trigger that arrives after the previous cycle finished.
func TestSchedulerRunsSequentialTriggersSeparately(t *testing.T) {
	fake := newFakeConverger()
	close(fake.release) // never block
	s := newScheduler(fake)

	for i := range 3 {
		result, err := s.Converge(t.Context(), TickFull)
		if err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
		if result.Coalesced {
			t.Fatalf("cycle %d reported Coalesced with no cycle in flight", i)
		}
	}
	if got := fake.count(); got != 3 {
		t.Fatalf("converger called %d times, want 3", got)
	}
}

// TestSchedulerPromotesPendingFullMode pins that a full sync requested during a
// namespace-only cycle is not silently lost: the NEXT cycle is promoted, so the
// materialization the caller asked for actually happens.
func TestSchedulerPromotesPendingFullMode(t *testing.T) {
	fake := newFakeConverger()
	s := newScheduler(fake)

	started := make(chan struct{})
	go func() {
		close(started)
		_, _ = s.Converge(t.Context(), TickNamespaceOnly)
	}()
	<-started
	<-fake.started

	joined := make(chan struct{})
	go func() {
		defer close(joined)
		_, _ = s.Converge(t.Context(), TickFull)
	}()
	time.Sleep(150 * time.Millisecond) // let the full request register as pending
	close(fake.release)
	<-joined

	// The next cycle must run as full, not namespace-only.
	if _, err := s.Converge(t.Context(), TickNamespaceOnly); err != nil {
		t.Fatalf("promoted cycle: %v", err)
	}
	modes := fake.seenModes()
	if len(modes) != 2 {
		t.Fatalf("modes = %v, want 2 cycles", modes)
	}
	if modes[0] != TickNamespaceOnly {
		t.Fatalf("first cycle mode = %q, want namespace-only", modes[0])
	}
	if modes[1] != TickFull {
		t.Fatalf("second cycle mode = %q, want the pending full request to have promoted it", modes[1])
	}
}

// TestSchedulerTracksFailuresForHealth pins the health accounting that
// /v1/health reports, including that a success clears the streak.
func TestSchedulerTracksFailuresForHealth(t *testing.T) {
	fake := newFakeConverger()
	close(fake.release)
	fake.err = errors.New("hub unreachable")
	s := newScheduler(fake)

	for range 3 {
		if _, err := s.Converge(t.Context(), TickFull); err == nil {
			t.Fatal("Converge succeeded, want the injected failure")
		}
	}
	health := s.snapshot()
	if health.Failures != 3 {
		t.Fatalf("failures = %d, want 3", health.Failures)
	}
	if health.LastErr == nil {
		t.Fatal("lastErr is nil after three failures")
	}
	if health.LastRunAt.IsZero() {
		t.Fatal("lastRunAt not recorded")
	}

	fake.err = nil
	if _, err := s.Converge(t.Context(), TickFull); err != nil {
		t.Fatalf("recovery cycle: %v", err)
	}
	health = s.snapshot()
	if health.Failures != 0 || health.LastErr != nil {
		t.Fatalf("after success: failures = %d, lastErr = %v; want 0 and nil", health.Failures, health.LastErr)
	}
}

// TestBackoffGrowsAndCaps pins the daemon's failure policy: back off, never
// exit, and stay responsive enough to notice recovery.
func TestBackoffGrowsAndCaps(t *testing.T) {
	interval := time.Minute
	if got := backoff(interval, 1); got != 2*time.Minute {
		t.Fatalf("backoff(1) = %v, want 2m", got)
	}
	if got := backoff(interval, 3); got != 8*time.Minute {
		t.Fatalf("backoff(3) = %v, want 8m", got)
	}
	if got := backoff(interval, 20); got != maxBackoff {
		t.Fatalf("backoff(20) = %v, want the %v cap — an hours-long outage must still retry promptly on recovery", got, maxBackoff)
	}
}

// TestSchedulerConvergeRespectsCallerCancellation pins that a joiner whose own
// context is cancelled stops waiting, rather than being pinned to a long cycle.
func TestSchedulerConvergeRespectsCallerCancellation(t *testing.T) {
	fake := newFakeConverger()
	s := newScheduler(fake)

	go func() { _, _ = s.Converge(context.WithoutCancel(t.Context()), TickFull) }()
	<-fake.started

	ctx, cancel := context.WithCancel(t.Context())
	joinErr := make(chan error, 1)
	go func() {
		_, err := s.Converge(ctx, TickFull)
		joinErr <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-joinErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("joiner err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled joiner never returned")
	}
	close(fake.release)
}

// TestRunPeriodicStopsOnCancel pins that the periodic loop is bounded by its
// context — a daemon shutting down must not leave it running.
func TestRunPeriodicStopsOnCancel(t *testing.T) {
	fake := newFakeConverger()
	close(fake.release)
	s := newScheduler(fake)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runPeriodic(ctx, 50*time.Millisecond, nil)
	}()

	// The loop converges immediately on entry rather than waiting a full
	// interval first.
	<-fake.started
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runPeriodic did not stop on cancel")
	}
}

// TestRunPeriodicDisabledAtZeroInterval pins that interval=0 means "on-demand
// only" rather than "converge in a hot loop".
func TestRunPeriodicDisabledAtZeroInterval(t *testing.T) {
	fake := newFakeConverger()
	close(fake.release)
	s := newScheduler(fake)

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runPeriodic(t.Context(), 0, nil)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runPeriodic with a zero interval should return immediately")
	}
	if got := fake.count(); got != 0 {
		t.Fatalf("converger called %d times at interval=0, want 0", got)
	}
}

// startServerWithConverger boots a real server wired to a converger, so the
// endpoint tests exercise the actual HTTP path rather than the scheduler alone.
func startServerWithConverger(t *testing.T, c Converger) string {
	t.Helper()
	socket := tempSocketPath(t)
	server, err := New(Config{SocketPath: socket, Version: "test", Logger: testLogger(), Converger: c})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	waitForSocket(t, socket)
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve returned %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("timeout waiting for Serve to return")
		}
	})
	return socket
}

// TestSyncEndpointReturns503WithoutConverger pins that a transport-only daemon
// says so plainly instead of pretending to converge.
func TestSyncEndpointReturns503WithoutConverger(t *testing.T) {
	socket := startServer(t, "test")
	client := rawClient(socket)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+socketHost+"/v1/sync", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

// TestHealthReportsConvergenceFailureWithoutClaimingDown pins the split between
// transport liveness and convergence health: a daemon whose syncs are failing
// still answers, and must report ok=true with healthy=false rather than looking
// dead to a supervisor.
func TestHealthReportsConvergenceFailureWithoutClaimingDown(t *testing.T) {
	fake := newFakeConverger()
	close(fake.release)
	fake.err = errors.New("hub unreachable at https://user:secret@hub.example/path")
	socket := startServerWithConverger(t, fake)

	client := rawClient(socket)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+socketHost+"/v1/sync", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("sync status = %d, want 500 for a failed convergence", resp.StatusCode)
	}

	health, err := NewClient(socket).Health(t.Context())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !health.OK {
		t.Fatal("ok = false; a daemon with failing convergence is still serving and must not report itself down")
	}
	if health.Healthy {
		t.Fatal("healthy = true after a failed convergence")
	}
	if health.ConsecutiveFailures == 0 {
		t.Fatal("consecutive_failures not reported")
	}
	if strings.Contains(health.LastError, "secret") {
		t.Fatalf("last_error leaked credentials: %q", health.LastError)
	}
}
