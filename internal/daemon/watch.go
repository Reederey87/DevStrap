package daemon

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Reederey87/DevStrap/internal/platform"
)

// WatchSource supplies the roots to watch. It is re-consulted on every
// (re)start of the watch plane, so a project adopted mid-run is picked up on the
// next restart rather than requiring a daemon bounce.
type WatchSource interface {
	WatchRoots(ctx context.Context) ([]string, error)
}

// watchState is what /v1/health and `daemon status` report about the watch
// plane. Degradation must be visible: a silently-degraded watcher would leave a
// user believing they have sub-interval convergence when they have none.
type watchState struct {
	// Backend is the adapter actually running ("fsnotify", "poll"), or empty
	// when the watch plane is not running at all.
	Backend string
	// Degraded is true when the native watcher failed and the plane fell back
	// to polling, or gave up entirely.
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

	mu    sync.Mutex
	state watchState
}

// logger is the tiny slice of *slog.Logger this file needs, so tests can assert
// on degradation without a log sink.
type logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}

func newWatchPlane(w, fallback platform.Watcher, src WatchSource, s *scheduler, log logger) *watchPlane {
	return &watchPlane{watcher: w, fallback: fallback, source: src, scheduler: s, logger: log}
}

func (p *watchPlane) snapshot() watchState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

func (p *watchPlane) setState(fn func(*watchState)) {
	p.mu.Lock()
	fn(&p.state)
	p.mu.Unlock()
}

// run watches the workspace until ctx is cancelled. It returns only on
// cancellation: every other failure degrades rather than propagating, because
// the daemon must keep serving and converging without a watcher.
func (p *watchPlane) run(ctx context.Context) {
	roots, err := p.source.WatchRoots(ctx)
	if err != nil {
		p.degrade("could not resolve watch roots: " + err.Error())
		return
	}
	if len(roots) == 0 {
		p.degrade("no materialized projects to watch yet")
		return
	}

	events := make(chan platform.FSEvent, 64)
	go p.consume(ctx, events)

	active := p.watcher
	name := "unknown"
	if active != nil {
		name = active.Name()
	}
	p.setState(func(s *watchState) {
		s.Backend = name
		s.Roots = len(roots)
		s.Degraded = false
		s.Reason = ""
	})
	p.logger.Info("daemon: watching workspace", "backend", name, "roots", len(roots))

	if err := p.watchAll(ctx, active, roots, events); err == nil || ctx.Err() != nil {
		return
	} else {
		// The native watcher failed for a real reason — most likely descriptor
		// exhaustion (EMFILE) or an inotify watch limit (ENOSPC) on a large
		// tree. Falling back to polling keeps sub-interval convergence working
		// at reduced fidelity instead of losing the plane entirely (PLAT-02).
		p.degrade("native watcher failed, polling instead: " + err.Error())
	}

	if p.fallback == nil {
		return
	}
	p.setState(func(s *watchState) { s.Backend = p.fallback.Name() })
	if err := p.watchAll(ctx, p.fallback, roots, events); err != nil && ctx.Err() == nil {
		p.degrade("polling watcher failed: " + err.Error())
	}
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
	p.setState(func(s *watchState) {
		s.Degraded = true
		s.Reason = reason
	})
	p.logger.Warn("daemon: watch plane degraded", "reason", reason)
}
