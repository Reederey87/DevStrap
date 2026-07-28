package daemon

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Reederey87/DevStrap/internal/redact"
)

// Converger runs one convergence cycle. It is the daemon's ONLY dependency on
// DevStrap's engine, and it is deliberately a single method: the daemon
// converges and reports, it does not hydrate one project, create a worktree, or
// run an agent on request.
//
// The implementation lives in internal/cli as a thin adapter over the existing
// runLoopTick, so `devstrap run-loop` and a daemon-driven cycle execute the
// identical code path — there is no second convergence engine to drift. See the
// ARCH2-01 narrowing in spec/03.
type Converger interface {
	Converge(ctx context.Context, mode TickMode) (Result, error)
}

// TickMode selects how much of a cycle to run.
type TickMode string

const (
	// TickFull is scan+adopt → sync → materialize: the whole cycle.
	TickFull TickMode = "full"
	// TickNamespaceOnly skips materialization. Watcher-driven ticks use this:
	// a filesystem hint must never cause DevStrap to clone repositories, which
	// is the code-level form of the Milestone 5 gate's "events are hints and do
	// not hydrate without explicit open/adopt" condition.
	TickNamespaceOnly TickMode = "namespace-only"
)

// Result summarizes one convergence cycle for the API.
type Result struct {
	Mode       TickMode  `json:"mode"`
	StartedAt  time.Time `json:"started_at"`
	DurationMS int64     `json:"duration_ms"`
	// Coalesced reports that this result was produced by a cycle already in
	// flight when the caller asked, rather than by a cycle started for them.
	Coalesced bool `json:"coalesced,omitempty"`
}

// ErrConvergeInFlight is never returned to callers; scheduler.Converge joins an
// in-flight cycle instead of rejecting. It exists so a future caller that wants
// try-lock semantics has a sentinel to compare against.
var ErrConvergeInFlight = errors.New("daemon: convergence already in flight")

// scheduler serializes convergence. At most one cycle runs at a time, and
// callers arriving during a cycle JOIN it rather than queueing another — a
// burst of watcher hints must produce one convergence, not one per hint.
//
// This is the honest, minimal subset of spec/13's designed job model: two job
// shapes (a periodic tick and an on-demand sync), single-flight, no persistence.
// The remaining eleven designed job types stay design intent.
type scheduler struct {
	converger Converger
	// events receives cycle lifecycle events. May be nil in tests that do not
	// care about them; every publish site must nil-check.
	events *eventBus

	mu       sync.Mutex
	inFlight *cycle
	// pending is set when a trigger arrives during a cycle whose mode is
	// weaker than the trigger wants. The next cycle is promoted to that mode so
	// a full sync requested during a namespace-only tick is not silently lost.
	pendingMode TickMode

	// last* record the most recent outcome for /v1/health.
	lastResult    Result
	lastErr       error
	lastRunAt     time.Time
	lastSuccessAt time.Time
	failures      int
}

// cycle is one in-flight convergence that late callers can wait on.
type cycle struct {
	mode   TickMode
	done   chan struct{}
	result Result
	err    error
}

func newScheduler(c Converger, events *eventBus) *scheduler {
	return &scheduler{converger: c, events: events}
}

// Converge runs a cycle, or joins the one already running.
//
// Joining rather than queueing is the point: N simultaneous triggers produce one
// convergence and N identical answers. A joined caller's result is marked
// Coalesced so it can tell that its request did not start the work it observed.
func (s *scheduler) Converge(ctx context.Context, mode TickMode) (Result, error) {
	s.mu.Lock()
	if existing := s.inFlight; existing != nil {
		// A stronger request arriving mid-cycle is remembered, so the work it
		// asked for happens on the next cycle rather than being dropped.
		if mode == TickFull && existing.mode != TickFull {
			s.pendingMode = TickFull
		}
		s.mu.Unlock()

		select {
		case <-existing.done:
			result := existing.result
			result.Coalesced = true
			return result, existing.err
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}

	if s.pendingMode == TickFull {
		mode = TickFull
		s.pendingMode = ""
	}
	current := &cycle{mode: mode, done: make(chan struct{})}
	s.inFlight = current
	s.mu.Unlock()

	s.publish(Event{Kind: EventConvergeStarted, At: time.Now()})

	started := time.Now()
	result, err := s.converger.Converge(ctx, mode)
	result.Mode = mode
	result.StartedAt = started
	result.DurationMS = time.Since(started).Milliseconds()

	s.mu.Lock()
	current.result, current.err = result, err
	s.inFlight = nil
	s.lastResult, s.lastErr, s.lastRunAt = result, err, started
	if err != nil {
		s.failures++
	} else {
		s.failures = 0
		s.lastSuccessAt = started
	}
	// Published under s.mu deliberately: it is what orders this cycle's
	// terminal event before the next cycle's `started`. Safe because
	// eventBus.publish is non-blocking and never calls back into the
	// scheduler, so the lock order is strictly scheduler -> bus. If that ever
	// stops being true, move this out and give Event a cycle id instead.
	if err != nil {
		s.publish(Event{Kind: EventConvergeFailed, At: time.Now(), Detail: redact.Scrub(err.Error())})
	} else {
		s.publish(Event{Kind: EventConvergeDone, At: time.Now()})
	}
	s.mu.Unlock()
	close(current.done)

	return result, err
}

// convergeHealth is the scheduler's contribution to /v1/health.
type convergeHealth struct {
	LastResult    Result
	LastRunAt     time.Time
	LastSuccessAt time.Time
	Failures      int
	LastErr       error
}

// snapshot reports the scheduler's health for /v1/health.
func (s *scheduler) snapshot() convergeHealth {
	s.mu.Lock()
	defer s.mu.Unlock()
	return convergeHealth{
		LastResult:    s.lastResult,
		LastRunAt:     s.lastRunAt,
		LastSuccessAt: s.lastSuccessAt,
		Failures:      s.failures,
		LastErr:       s.lastErr,
	}
}

func (s *scheduler) publish(e Event) {
	if s.events != nil {
		s.events.publish(e)
	}
}

// runPeriodic converges on an interval until the context is cancelled.
//
// The failure policy deliberately differs from `run-loop`'s. run-loop exits
// after runLoopMaxConsecutiveFailures so its supervisor restarts it; a daemon
// must NOT exit on transient failure, because it is also serving reads and a
// restart loop would make those flap. It backs off instead, reports
// healthy=false with the last error on /v1/health, and keeps serving.
func (s *scheduler) runPeriodic(ctx context.Context, interval time.Duration, jitter func(time.Duration) time.Duration) {
	if interval <= 0 {
		return
	}
	for {
		// Converge immediately on entry, then wait — a daemon that just started
		// should not sit idle for a full interval before doing anything.
		if _, err := s.Converge(ctx, TickFull); err != nil && ctx.Err() != nil {
			return
		}

		health := s.snapshot()
		wait := interval
		if health.Failures > 0 {
			wait = backoff(interval, health.Failures)
		}
		if jitter != nil {
			wait = jitter(wait)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// maxBackoff caps the failure backoff so a daemon that has been failing for
// hours still retries often enough to notice recovery promptly.
const maxBackoff = 30 * time.Minute

// backoff grows the wait exponentially with consecutive failures, capped. It
// keeps a persistently-failing daemon from hammering an unreachable hub while
// still recovering within one capped interval once the hub returns.
func backoff(interval time.Duration, failures int) time.Duration {
	wait := interval
	for range failures {
		wait *= 2
		if wait >= maxBackoff {
			return maxBackoff
		}
	}
	return wait
}
