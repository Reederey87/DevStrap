package cli

import (
	"context"
	"io"
	"os"

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
	// A TickNamespaceOnly cycle is what the watcher's hints produce, and only
	// what they produce (watch.go floors them at 5s and always requests this
	// mode). Skip the full-workspace scan+adopt for it — P9-DAEMON-02 — so a
	// hint-driven convergence is genuinely cheaper than the periodic tick it
	// pre-empts, which is the entire cost argument the Milestone 5 entry gate
	// made for having a watcher at all. A daemon configured namespace-only for
	// other reasons (forceNamespaceOnly) keeps scanning.
	skipScanAdopt := mode == daemon.TickNamespaceOnly
	err := runLoopTickOpts(ctx, c.stdout, c.stderr, c.opts, c.hubFile, namespaceOnly, true, skipScanAdopt)
	return daemon.Result{}, err
}

// cliWatchSource supplies the daemon's watch roots: the local paths of projects
// that are actually materialized on this device.
//
// Watching per-project roots rather than the whole workspace root is deliberate.
// The workspace root contains everything, including trees DevStrap does not
// manage, and on kqueue (macOS) every watched entry costs a file descriptor —
// so a blanket watch is both noisier and more expensive than the set of paths
// whose changes actually mean something to the namespace.
type cliWatchSource struct {
	opts *options
}

func (c cliWatchSource) WatchRoots(ctx context.Context) ([]string, error) {
	store, err := c.opts.openState(ctx)
	if err != nil {
		return nil, err
	}
	defer closeStore(store)

	projects, err := store.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	roots := make([]string, 0, len(projects))
	for _, p := range projects {
		// A skeleton has no content to watch yet; it becomes watchable after
		// the next materialization, which the periodic cycle performs.
		if p.LocalPath == "" || p.MaterializationState == "skeleton" {
			continue
		}
		if _, statErr := os.Stat(p.LocalPath); statErr != nil {
			continue
		}
		roots = append(roots, p.LocalPath)
	}
	return roots, nil
}

// cliReader adapts the local store to the daemon's Reader seam, backing
// GET /v1/status.
//
// Note what this deliberately is NOT: a query API. It returns the same summary
// `devstrap status` prints and nothing more. A consumer wanting per-project
// detail opens the store itself — making the daemon a database proxy would give
// it a second, drifting view of state it does not own.
type cliReader struct {
	opts *options
}

func (c cliReader) Status(ctx context.Context) (daemon.Status, error) {
	store, err := c.opts.openState(ctx)
	if err != nil {
		return daemon.Status{}, err
	}
	defer closeStore(store)

	summary, err := store.Summary(ctx)
	if err != nil {
		return daemon.Status{}, err
	}
	return daemon.Status{
		WorkspaceName: summary.WorkspaceName,
		WorkspaceID:   summary.WorkspaceID,
		RootPath:      summary.RootPath,
		ProjectCount:  summary.ProjectCount,
		DeviceID:      summary.DeviceID,
	}, nil
}
