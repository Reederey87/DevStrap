package daemon

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/redact"
)

// WatchSource supplies the roots to watch. It is re-consulted periodically
// while the plane runs, so projects materialized after daemon startup are
// discovered without restarting the daemon.
type WatchSource interface {
	WatchRoots(ctx context.Context) ([]string, error)
}

// watchPhase is what the plane is actually doing. It exists because a boolean
// `degraded` cannot distinguish "nothing to watch yet" (a brand-new workspace,
// entirely normal) from "the watcher failed" (worth alarming on) — and
// conflating them makes the alarm useless on day one.
type watchPhase string

const (
	// watchPhaseStarting is the state before the plane has resolved roots even
	// once. It exists because the zero value of watchPhase is "", and an empty
	// string on /v1/health is precisely the ambiguity this tri-state removes:
	// a consumer could not tell "not started yet" from "never reported". The
	// window is real — WatchRoots opens SQLite — so it needs a name.
	watchPhaseStarting watchPhase = "starting" // roots not resolved yet
	watchPhaseIdle     watchPhase = "idle"     // no materialized projects yet
	watchPhaseWatching watchPhase = "watching" // native watcher armed
	watchPhaseDegraded watchPhase = "degraded" // watcher failed; polling or nothing
)

// watchState is what /v1/health and `daemon status` report about the watch
// plane. Degradation must be visible: a silently-degraded watcher would leave a
// user believing they have sub-interval convergence when they have none.
type watchState struct {
	// Phase distinguishes normal idleness from native watching and degradation.
	Phase watchPhase
	// Backend is the adapter actually running ("fsnotify", "poll"), or empty
	// when the watch plane is not running at all.
	Backend string
	// Degraded is retained for existing consumers and is always derived from
	// Phase.
	Degraded bool
	// Reason explains the degradation in one line.
	Reason string
	// Roots is how many paths are being watched — the measurement the FSEvents
	// plane arms, not the recursive directory total.
	Roots int
	// WatchedDirs is the live recursive directory total when the active adapter
	// can report it. DirsKnown distinguishes a genuine zero from unknown.
	WatchedDirs int
	DirsKnown   bool
	// Hints counts coalesced triggers observed since start.
	Hints   uint64
	counter platform.WatchedDirCounter
}

// minTriggerInterval floors the gap between watcher-driven convergences. The
// adapter already debounces at ~250ms, but a debounce bounds burst-to-hint, not
// hint-to-convergence: without this floor, a save-storm that outlasts one
// convergence would start another the moment it finished.
const minTriggerInterval = 5 * time.Second

const (
	defaultRediscoverInterval = 60 * time.Second
	// A Watch call is long-lived. Surviving this short window distinguishes an
	// armed native watcher from adapters that reject an arm immediately.
	nativeArmReadyDelay = 10 * time.Millisecond

	// maxRecoveryProbation caps how long a retry from degraded waits before
	// claiming recovery. Recovery visibility delayed by up to this long is a
	// far better trade than a false "recovered" every retry.
	maxRecoveryProbation = 30 * time.Second
)

// watchPlane turns filesystem hints into convergence triggers.
//
// Two invariants make this safe, and both are load-bearing for the Milestone 5
// entry gate (spec/14):
//
//  1. A hint NEVER hydrates. Watcher-driven convergence always runs
//     TickNamespaceOnly — scan+adopt+sync, no materialization. An FSEvent cannot
//     name the file that changed (it carries only the watch root), so there is
//     nothing to hydrate FROM, and this makes that structural fact operational:
//     no filesystem activity, hostile or accidental, can cause DevStrap to clone
//     repositories. Materialization stays on the periodic cycle and on explicit
//     POST /v1/sync.
//  2. The watcher is an optimization, never the guarantee. If it fails, dies, or
//     is unsupported, periodic convergence still runs and correctness is
//     unaffected — only latency degrades. That is what licenses degrading to
//     polling instead of failing the daemon (PLAT-02/PLAT-03).
type watchPlane struct {
	watcher   platform.Watcher
	fallback  platform.Watcher
	source    WatchSource
	scheduler *scheduler
	logger    logger
	events    *eventBus

	rediscoverInterval time.Duration

	mu    sync.Mutex
	state watchState
	// probationArm is the generation of the arm currently serving a recovery
	// probation, or 0 for none. It is control state, deliberately not part of
	// watchState: nothing reports it, and the re-discovery loop is its only
	// consumer. A generation rather than a boolean because a cancelled arm and
	// its replacement overlap briefly — the outgoing arm must not clear a flag
	// the incoming one just set.
	probationArm uint64
}

// logger is the tiny slice of *slog.Logger this file needs, so tests can assert
// on degradation without a log sink.
type logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}

func newWatchPlane(w, fallback platform.Watcher, src WatchSource, s *scheduler, log logger, events *eventBus) *watchPlane {
	return &watchPlane{
		watcher:            w,
		fallback:           fallback,
		source:             src,
		scheduler:          s,
		logger:             log,
		events:             events,
		rediscoverInterval: defaultRediscoverInterval,
		state:              watchState{Phase: watchPhaseStarting},
	}
}

// snapshot reads the plane's state plus, when the armed backend can report it,
// the live watched-directory count.
//
// The counter call happens OUTSIDE p.mu — it reaches into the platform adapter,
// which takes its own lock, and holding both would couple this plane's lock to
// an adapter's. That leaves a window in which a concurrent degrade(), idle(), or
// poll fallback clears the counter while the call is in flight, so the result is
// RE-VALIDATED under the lock before publication: a count is reported only if
// the plane is still armed on the very same counter it was read from. Otherwise
// the field stays unknown, which is the honest answer — a degraded or polling
// plane must never carry a count from the native arm that preceded it.
func (p *watchPlane) snapshot() watchState {
	p.mu.Lock()
	state := p.state
	counter := state.counter
	p.mu.Unlock()
	if counter == nil {
		return state
	}
	dirs := counter.WatchedDirs()
	p.mu.Lock()
	defer p.mu.Unlock()
	state = p.state
	if state.counter != counter {
		return state
	}
	state.WatchedDirs = dirs
	state.DirsKnown = true
	return state
}

// beginProbation marks gen as the arm serving a recovery probation.
func (p *watchPlane) beginProbation(gen uint64) {
	p.mu.Lock()
	p.probationArm = gen
	p.mu.Unlock()
}

// endProbation clears the probation only if gen still owns it, so a cancelled
// arm cannot clear the probation of the arm that replaced it.
func (p *watchPlane) endProbation(gen uint64) {
	p.mu.Lock()
	if p.probationArm == gen {
		p.probationArm = 0
	}
	p.mu.Unlock()
}

// needsRearm decides whether the re-discovery loop should replace the current
// arm. Reading the phase and the probation together under one lock is the point:
// a degraded plane retries native on every cadence, EXCEPT when the arm it
// already has IS that retry and is still serving its probation. Cancelling that
// arm would restart the probation on every tick — and since a probation is one
// full interval, the tick would always win, so a genuinely recovered watcher
// would be torn down and rebuilt forever without ever announcing recovery.
func (p *watchPlane) needsRearm(current, roots []string, armAlive bool) bool {
	if !armAlive || !sameRoots(current, roots) {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state.Phase == watchPhaseDegraded && p.probationArm == 0
}

func (p *watchPlane) setState(fn func(*watchState)) {
	p.mu.Lock()
	fn(&p.state)
	p.state.Degraded = p.state.Phase == watchPhaseDegraded
	p.mu.Unlock()
}

// run watches the workspace until ctx is cancelled. It returns only on
// cancellation: every other failure degrades rather than propagating, because
// the daemon must keep serving and converging without a watcher.
func (p *watchPlane) run(ctx context.Context) {
	events := make(chan platform.FSEvent, 64)
	go p.consume(ctx, events)

	interval := p.rediscoverInterval
	if interval <= 0 {
		interval = defaultRediscoverInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var current []string
	var cancelArm context.CancelFunc
	var armDone chan struct{}
	var armGen uint64
	// armAlive reports whether the arm we last started is still running. A
	// finished arm has closed its channel.
	armAlive := func() bool {
		if armDone == nil {
			return false
		}
		select {
		case <-armDone:
			return false
		default:
			return true
		}
	}
	stopArm := func() {
		if cancelArm != nil {
			cancelArm()
			cancelArm = nil
		}
		armDone = nil
	}
	defer stopArm()

	for {
		roots, err := p.source.WatchRoots(ctx)
		switch {
		case err != nil:
			// A store blip must not tear down a healthy watcher. Resolving
			// roots opens SQLite, so a busy timeout here says nothing about
			// the arm — which is still watching the same roots it was given.
			// Flipping to degraded would publish a false alarm AND, because
			// the degraded phase forces a re-arm on the next tick, cancel and
			// rebuild every descriptor for no reason.
			if armAlive() {
				p.logger.Warn("daemon: could not resolve watch roots; keeping the current watcher",
					"error", err.Error())
				break
			}
			p.degrade("could not resolve watch roots: " + err.Error())
		case len(roots) == 0:
			current = nil
			stopArm()
			p.idle()
		default:
			if p.needsRearm(current, roots, armAlive()) {
				if cancelArm != nil {
					cancelArm()
				}
				current = append([]string(nil), roots...)
				armCtx, cancel := context.WithCancel(ctx)
				cancelArm = cancel
				done := make(chan struct{})
				armDone = done
				armGen++
				gen := armGen
				go func() {
					defer close(done)
					p.arm(armCtx, gen, roots, events)
				}()
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// arm tries the native watcher first, then retains the existing polling
// fallback semantics after a real (non-context) native failure.
//
// There is no success signal to wait for: platform.Watcher.Watch blocks until
// it errors or is cancelled, so "armed" can only ever be inferred from "has not
// failed yet". How long to wait before inferring it is the whole subtlety.
//
// From a HEALTHY state a short delay is right — the arm is almost certainly
// fine and the daemon should report so promptly. Retrying from DEGRADED is the
// opposite case: the motivating failure is descriptor exhaustion on a large
// tree, where addRecursiveWatch walks thousands of directories before hitting
// the limit — far longer than the short delay. Declaring "watching" after
// 10ms there would publish a false recovery, then a fresh degrade when the walk
// finally failed, once per retry: precisely the flapping this plane exists to
// eliminate. So a retry from degraded must SURVIVE a probation before it may
// claim recovery.
func (p *watchPlane) arm(ctx context.Context, gen uint64, roots []string, events chan<- platform.FSEvent) {
	wasDegraded := p.snapshot().Phase == watchPhaseDegraded
	ready := nativeArmReadyDelay
	if wasDegraded {
		ready = p.recoveryProbation()
		// Claim the probation so the re-discovery loop leaves this arm alone
		// until the probation resolves, one way or the other.
		p.beginProbation(gen)
	}

	nativeResult := make(chan error, 1)
	go func() { nativeResult <- p.watchAll(ctx, p.watcher, roots, events) }()

	timer := time.NewTimer(ready)
	defer timer.Stop()
	select {
	case err := <-nativeResult:
		// The probation is over the moment its outcome is known — not when this
		// function returns, which for a failed native arm is after the polling
		// fallback finally exits. Holding the probation across the polling phase
		// would suppress every future native retry.
		p.endProbation(gen)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			// The watcher exited on its own without an error and without
			// cancellation. Nothing is armed, so continuing to report the
			// previous phase would be a liveness lie that no later tick
			// corrects (unchanged roots + non-degraded phase never re-arms).
			p.degrade("native watcher exited unexpectedly without an error")
			return
		}
		p.degrade("native watcher failed, polling instead: " + err.Error())
	case <-timer.C:
		p.endProbation(gen)
		name := "unknown"
		if p.watcher != nil {
			name = p.watcher.Name()
		}
		p.watching(p.watcher, len(roots))
		p.logger.Info("daemon: watching workspace", "backend", name, "roots", len(roots))
		err := <-nativeResult
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			p.degrade("native watcher exited unexpectedly without an error")
			return
		}
		p.degrade("native watcher failed, polling instead: " + err.Error())
	case <-ctx.Done():
		p.endProbation(gen)
		return
	}

	if p.fallback == nil || ctx.Err() != nil {
		return
	}
	p.setState(func(s *watchState) {
		s.Backend = p.fallback.Name()
		s.Roots = len(roots)
		s.counter = nil
		s.WatchedDirs = 0
		s.DirsKnown = false
	})
	if err := p.watchAll(ctx, p.fallback, roots, events); err != nil && ctx.Err() == nil {
		p.degrade("polling watcher failed: " + err.Error())
	}
}

// recoveryProbation is how long a retry from degraded must survive before it
// may claim recovery. It scales with the re-discovery interval — the cadence is
// the only in-band estimate of how patient this plane should be — and is floored
// so a fast test interval still exercises the probation path rather than
// skipping it.
//
// A probation may therefore be as long as, or longer than, the interval that
// scheduled it. That is deliberate and safe only because the loop refuses to
// re-arm over a live probation (see needsRearm): a probation that raced the next
// tick would be cancelled and restarted forever, and recovery would never be
// announced at all.
func (p *watchPlane) recoveryProbation() time.Duration {
	interval := p.rediscoverInterval
	if interval <= 0 {
		interval = defaultRediscoverInterval
	}
	// One FULL re-discovery interval, not a fraction. The probation has to
	// outlast how long an arm takes to fail, and that latency is unbounded in
	// principle — addRecursiveWatch walks the whole tree before hitting the
	// limit. In production the interval is 60s against a walk that fails in
	// seconds, so a full interval clears it comfortably; half an interval
	// narrows the margin for no benefit, since nothing depends on announcing
	// recovery sooner.
	//
	// The cap keeps recovery visibility bounded when the cadence is long.
	if interval > maxRecoveryProbation {
		return maxRecoveryProbation
	}
	if interval < nativeArmReadyDelay {
		return nativeArmReadyDelay
	}
	return interval
}

func sameRoots(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	a = append([]string(nil), a...)
	b = append([]string(nil), b...)
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// watchAll runs one watcher across every root, returning the first non-context
// error. Roots are watched concurrently because platform.Watcher watches a
// single tree per call.
func (p *watchPlane) watchAll(ctx context.Context, w platform.Watcher, roots []string, events chan<- platform.FSEvent) error {
	if w == nil {
		return errors.New("no watcher adapter available on this platform")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errs := make(chan error, len(roots))
	for _, root := range roots {
		go func() { errs <- w.Watch(ctx, root, events) }()
	}
	// P9-DAEMON-01: a per-root failure must not take the whole plane down. This
	// previously returned on the FIRST non-context error, and the deferred
	// cancel then killed every other root's watch — so one project with an
	// unreadable directory silently disabled native watching for every unrelated
	// project on the device. Collect instead, and degrade only when NO root is
	// watchable; a partial plane is strictly better than none, since the
	// remaining roots keep their sub-interval latency and the periodic cycle
	// still covers the failed one.
	var failed int
	var firstErr error
	for range roots {
		err := <-errs
		if err != nil && !errors.Is(err, context.Canceled) {
			failed++
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if failed > 0 && failed == len(roots) {
		return firstErr
	}
	return nil
}

// consume converts hints into convergence triggers, floored by
// minTriggerInterval and always namespace-only.
func (p *watchPlane) consume(ctx context.Context, events <-chan platform.FSEvent) {
	var last time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-events:
			if !ok {
				return
			}
			p.setState(func(s *watchState) { s.Hints++ })

			if since := time.Since(last); since < minTriggerInterval {
				// Drop rather than queue: the next hint will trigger, and a
				// dropped hint costs at most one interval of latency because
				// periodic convergence is still running underneath.
				continue
			}
			last = time.Now()

			// TickNamespaceOnly is the invariant: a filesystem hint scans,
			// adopts, and syncs, but never materializes.
			if _, err := p.scheduler.Converge(ctx, TickNamespaceOnly); err != nil && ctx.Err() == nil {
				p.logger.Warn("daemon: watcher-triggered convergence failed", "error", err.Error())
			}
		}
	}
}

// degrade, idle and watching publish INSIDE the state lock, for the same reason
// scheduler.Converge publishes its terminal events under its own: it is what
// orders the events consistently with the states they describe. Published after
// the unlock, a concurrent transition can interleave and a consumer sees
// "recovered" then "degraded" while the settled state is watching. Safe because
// eventBus.publish is non-blocking and never calls back into the plane, so the
// lock order is strictly plane -> bus.
func (p *watchPlane) degrade(reason string) {
	p.transition(watchPhaseDegraded, func(s *watchState) {
		s.Reason = reason
		s.counter = nil
		s.WatchedDirs = 0
		s.DirsKnown = false
	}, redact.Scrub(reason))
	p.logger.Warn("daemon: watch plane degraded", "reason", reason)
}

func (p *watchPlane) idle() {
	p.transition(watchPhaseIdle, func(s *watchState) {
		s.Backend = ""
		s.Reason = "no materialized projects yet"
		s.Roots = 0
		s.counter = nil
		s.WatchedDirs = 0
		s.DirsKnown = false
	}, "watch plane recovered: idle")
}

func (p *watchPlane) watching(watcher platform.Watcher, roots int) {
	p.transition(watchPhaseWatching, func(s *watchState) {
		s.Backend = watcher.Name()
		s.Reason = ""
		s.Roots = roots
		s.counter, _ = watcher.(platform.WatchedDirCounter)
		s.WatchedDirs = 0
		s.DirsKnown = false
	}, "watch plane recovered: native watcher armed")
}

// transition moves the plane to phase, applying fields, and publishes exactly
// once when the degraded-ness actually CHANGED — entering degraded, or leaving
// it. A plane that re-fails while already degraded publishes nothing, which is
// what keeps a flapping watcher from becoming its own event storm on a stream
// documented as lossy.
func (p *watchPlane) transition(phase watchPhase, fields func(*watchState), detail string) {
	p.mu.Lock()
	was := p.state.Phase
	p.state.Phase = phase
	fields(&p.state)
	p.state.Degraded = p.state.Phase == watchPhaseDegraded
	if (was == watchPhaseDegraded) != (phase == watchPhaseDegraded) {
		p.publish(Event{Kind: EventWatchDegraded, At: time.Now(), Detail: detail})
	}
	p.mu.Unlock()
}

func (p *watchPlane) publish(e Event) {
	if p.events != nil {
		p.events.publish(e)
	}
}
