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
	// decision in spec/05 is gated on.
	Roots int
	// Hints counts coalesced triggers observed since start.
	Hints uint64
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

func (p *watchPlane) snapshot() watchState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
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
	defer func() {
		if cancelArm != nil {
			cancelArm()
		}
	}()

	for {
		roots, err := p.source.WatchRoots(ctx)
		switch {
		case err != nil:
			p.degrade("could not resolve watch roots: " + err.Error())
		case len(roots) == 0:
			current = nil
			if cancelArm != nil {
				cancelArm()
				cancelArm = nil
			}
			p.idle()
		default:
			if !sameRoots(current, roots) || p.snapshot().Phase == watchPhaseDegraded {
				if cancelArm != nil {
					cancelArm()
				}
				current = append([]string(nil), roots...)
				armCtx, cancel := context.WithCancel(ctx)
				cancelArm = cancel
				go p.arm(armCtx, roots, events)
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
func (p *watchPlane) arm(ctx context.Context, roots []string, events chan<- platform.FSEvent) {
	nativeResult := make(chan error, 1)
	go func() { nativeResult <- p.watchAll(ctx, p.watcher, roots, events) }()

	timer := time.NewTimer(nativeArmReadyDelay)
	defer timer.Stop()
	select {
	case err := <-nativeResult:
		if err == nil || ctx.Err() != nil {
			return
		}
		p.degrade("native watcher failed, polling instead: " + err.Error())
	case <-timer.C:
		name := "unknown"
		if p.watcher != nil {
			name = p.watcher.Name()
		}
		p.watching(name, len(roots))
		p.logger.Info("daemon: watching workspace", "backend", name, "roots", len(roots))
		err := <-nativeResult
		if err == nil || ctx.Err() != nil {
			return
		}
		p.degrade("native watcher failed, polling instead: " + err.Error())
	case <-ctx.Done():
		return
	}

	if p.fallback == nil || ctx.Err() != nil {
		return
	}
	p.setState(func(s *watchState) {
		s.Backend = p.fallback.Name()
		s.Roots = len(roots)
	})
	if err := p.watchAll(ctx, p.fallback, roots, events); err != nil && ctx.Err() == nil {
		p.degrade("polling watcher failed: " + err.Error())
	}
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
	for range roots {
		err := <-errs
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
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

func (p *watchPlane) degrade(reason string) {
	changed := false
	p.setState(func(s *watchState) {
		changed = s.Phase != watchPhaseDegraded
		s.Phase = watchPhaseDegraded
		s.Reason = reason
	})
	p.logger.Warn("daemon: watch plane degraded", "reason", reason)
	if changed {
		p.publish(Event{Kind: EventWatchDegraded, At: time.Now(), Detail: redact.Scrub(reason)})
	}
}

func (p *watchPlane) idle() {
	recovered := false
	p.setState(func(s *watchState) {
		recovered = s.Phase == watchPhaseDegraded
		s.Phase = watchPhaseIdle
		s.Backend = ""
		s.Reason = "no materialized projects yet"
		s.Roots = 0
	})
	if recovered {
		p.publish(Event{Kind: EventWatchDegraded, At: time.Now(), Detail: "watch plane recovered: idle"})
	}
}

func (p *watchPlane) watching(backend string, roots int) {
	recovered := false
	p.setState(func(s *watchState) {
		recovered = s.Phase == watchPhaseDegraded
		s.Phase = watchPhaseWatching
		s.Backend = backend
		s.Reason = ""
		s.Roots = roots
	})
	if recovered {
		p.publish(Event{Kind: EventWatchDegraded, At: time.Now(), Detail: "watch plane recovered: native watcher armed"})
	}
}

func (p *watchPlane) publish(e Event) {
	if p.events != nil {
		p.events.publish(e)
	}
}
