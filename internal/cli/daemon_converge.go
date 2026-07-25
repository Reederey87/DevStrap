package cli

import (
	"context"
	"io"

	"github.com/Reederey87/DevStrap/internal/daemon"
)

// cliConverger adapts the existing run-loop tick to the daemon's Converger
// interface.
//
// This is the whole of the ARCH2-01 narrowing (spec/03): rather than extracting
// an `internal/engine` package, the daemon depends on a one-method interface and
// this adapter calls `runLoopTick` — the exact function `devstrap run-loop`
// calls. There is no second convergence path to drift, and the dependency arrow
// points daemon → core, never daemon → cobra.
type cliConverger struct {
	opts   *options
	stdout io.Writer
	stderr io.Writer
	// hubFile mirrors run-loop's --hub-file test override.
	hubFile string
	// forceNamespaceOnly makes every cycle skip materialization, mirroring
	// run-loop's --namespace-only. It is an operator choice, distinct from the
	// per-tick TickNamespaceOnly mode a watcher hint will request.
	forceNamespaceOnly bool
}

// Converge runs one tick. The `once` argument to runLoopTick is true: it makes
// maintenance-lock contention a returned error rather than a skipped-and-warned
// cycle, which is what the daemon wants — the scheduler surfaces it as an
// unhealthy convergence rather than silently doing nothing.
//
// The lock is taken and released per tick inside runLoopTick, never held for the
// daemon's lifetime, so `db backup --full`, `db restore`, and `db down` keep
// working against a running daemon exactly as they do against run-loop.
func (c cliConverger) Converge(ctx context.Context, mode daemon.TickMode) (daemon.Result, error) {
	namespaceOnly := c.forceNamespaceOnly || mode == daemon.TickNamespaceOnly
	err := runLoopTick(ctx, c.stdout, c.stderr, c.opts, c.hubFile, namespaceOnly, true)
	return daemon.Result{}, err
}
