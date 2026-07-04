package cli

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"time"

	"github.com/Reederey87/DevStrap/internal/scan"
	"github.com/spf13/cobra"
)

// runLoopMaxConsecutiveFailures bounds how many back-to-back tick failures the
// foreground loop tolerates before it exits non-zero (P5-CLI-05). A scheduler
// or log consumer must be able to notice a persistently failing loop; swallowing
// every error forever hides outages. A single transient failure resets on the
// next success.
const runLoopMaxConsecutiveFailures = 5

// newRunLoopCommand implements `devstrap run-loop` (XP-02): a portable
// foreground ticker that runs scan → sync → materialize on an interval,
// identically on macOS and Ubuntu, without a native daemon. Any OS scheduler
// (cron, a user crontab, Task Scheduler, or a later launchd/systemd unit) can
// drive it with --once; the foreground loop delivers periodic convergence now.
func newRunLoopCommand(stdout io.Writer, opts *options) *cobra.Command {
	var hubFile string
	var interval time.Duration
	var once bool
	var namespaceOnly bool
	cmd := &cobra.Command{
		Use:   "run-loop",
		Short: "Run scan + sync + materialize on an interval (portable, no daemon)",
		RunE: func(cmd *cobra.Command, args []string) error {
			// P5-CLI-05: progress/diagnostics go to stderr so a scheduler or log
			// consumer can keep stdout (the sync result stream) clean.
			stderr := cmd.ErrOrStderr()
			// P5-HUB-01: fail fast if no hub is resolvable before starting the loop.
			// run-loop has no store open at preflight time, so validate config
			// without building the R2 adapter (hubConfigured, not hubFromOptions).
			if err := hubConfigured(opts, hubFile); err != nil {
				return appError{code: exitInvalidConfig, err: err}
			}
			if once {
				return runLoopTick(cmd.Context(), stdout, stderr, opts, hubFile, namespaceOnly)
			}
			// XP-02: foreground loop with jittered backoff. Graceful shutdown
			// on context cancellation (Ctrl-C / scheduler stop).
			return runLoopForever(cmd.Context(), stdout, stderr, opts, hubFile, namespaceOnly, interval)
		},
	}
	cmd.Flags().StringVar(&hubFile, "hub-file", "", "file-backed test hub path")
	cmd.Flags().DurationVar(&interval, "interval", 5*time.Minute, "time between sync cycles")
	cmd.Flags().BoolVar(&once, "once", false, "run a single sync cycle and exit (for cron)")
	cmd.Flags().BoolVar(&namespaceOnly, "namespace-only", false, "sync namespace metadata only; skip materialization")
	return cmd
}

// runLoopJitterBound returns the upper bound for rand.Int64N when computing the
// per-tick jitter (P5-QUAL-03). It is clamped to at least 1 so rand.Int64N never
// panics on sub-10ns intervals (where interval/10 would be 0).
func runLoopJitterBound(interval time.Duration) int64 {
	bound := int64(interval) / 10
	if bound < 1 {
		bound = 1
	}
	return bound
}

func runLoopTick(ctx context.Context, stdout, stderr io.Writer, opts *options, hubFile string, namespaceOnly bool) error {
	// P5-CLI-05: the tick header is progress, not a result — route it to stderr.
	opts.progressf(stderr, "[%s] run-loop tick: scan + sync + materialize\n", time.Now().UTC().Format(time.RFC3339))
	// P6-XP-03: scan + adopt at the START of each tick. Without a daemon or
	// watcher there is no other automatic local→hub path, so a project created
	// on this device would never reach the namespace (and thus the hub) until a
	// manual `scan --adopt`. The scan runs even under --namespace-only because it
	// FEEDS the namespace. P6-XP-05 made scan offline, so the per-tick cost is a
	// filesystem walk plus local git ref reads — cheap enough to run every tick.
	if err := runLoopScanAdopt(ctx, stderr, opts); err != nil {
		return err
	}
	return runSyncCycle(ctx, stdout, opts, hubFile, namespaceOnly, false)
}

// runLoopScanAdopt walks the workspace root and idempotently adopts any newly
// discovered projects (P6-XP-03). It is the loop's local→namespace feeder:
//
//   - Adoption is idempotent (adoptNewFindings): only genuinely new projects
//     emit a project.added event, so an unchanged tree adopts nothing on every
//     subsequent tick — no duplicate events.
//   - Fail-safe: warning-class items (secret-looking files, escaping symlinks,
//     duplicate remotes, per-finding warnings) are routed to stderr and never
//     auto-adopted. Secrets and escaping symlinks are excluded from
//     result.Findings by scan.Walk itself; findings whose remote is duplicated
//     across paths are ambiguous, so the unattended loop refuses to guess a
//     canonical path and drops them here (the one-shot `scan --adopt` still
//     adopts them — a deliberate operator action).
func runLoopScanAdopt(ctx context.Context, stderr io.Writer, opts *options) error {
	rootAbs, err := cleanAbsPath(opts.paths().Root)
	if err != nil {
		return appError{code: exitInvalidConfig, err: err}
	}
	result, err := scan.Walk(ctx, rootAbs, scan.Options{IncludePlainFolders: true})
	if err != nil {
		return err
	}
	// P6-XP-03 fail-safe: surface every warning-class signal on stderr; none of
	// these are auto-adopted.
	for _, w := range result.Warnings {
		_, _ = fmt.Fprintf(stderr, "scan warning: %s\n", w)
	}
	duplicated := map[string]bool{}
	for _, d := range result.Duplicates {
		duplicated[d.RemoteKey] = true
		_, _ = fmt.Fprintf(stderr, "scan warning: duplicate remote %s across %v (not auto-adopted; recommended %s)\n", d.RemoteKey, d.Paths, d.RecommendedPath)
	}
	for _, f := range result.Findings {
		for _, w := range f.Warnings {
			_, _ = fmt.Fprintf(stderr, "scan warning: %s: %s\n", f.Path, w)
		}
	}
	// Drop findings whose remote is duplicated across paths before adoption: the
	// loop never auto-adopts an ambiguous duplicate remote.
	adoptable := scan.Result{Findings: make([]scan.Finding, 0, len(result.Findings))}
	for _, f := range result.Findings {
		if f.Type == scan.TypeGitRepo && duplicated[f.RemoteKey] {
			continue
		}
		adoptable.Findings = append(adoptable.Findings, f)
	}
	store, err := opts.openState(ctx)
	if err != nil {
		return err
	}
	defer closeStore(store)
	adopted, err := adoptNewFindings(ctx, store, rootAbs, adoptable)
	if err != nil {
		return err
	}
	if adopted > 0 {
		opts.progressf(stderr, "scan adopted %d new project(s)\n", adopted)
	}
	return nil
}

func runLoopForever(ctx context.Context, stdout, stderr io.Writer, opts *options, hubFile string, namespaceOnly bool, interval time.Duration) error {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	consecutiveFailures := 0
	// tick runs one cycle, tracks consecutive failures, and returns a terminal
	// error once the loop has failed too many times in a row (P5-CLI-05).
	tick := func() error {
		if err := runLoopTick(ctx, stdout, stderr, opts, hubFile, namespaceOnly); err != nil {
			consecutiveFailures++
			_, _ = fmt.Fprintf(stderr, "run-loop tick error (%d consecutive): %v\n", consecutiveFailures, err)
			if consecutiveFailures >= runLoopMaxConsecutiveFailures {
				return appError{code: exitGeneric, err: fmt.Errorf("run-loop aborted after %d consecutive tick failures: %w", consecutiveFailures, err)}
			}
			return nil
		}
		consecutiveFailures = 0
		return nil
	}

	// Run one tick immediately on startup, then on the interval.
	if err := tick(); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			_, _ = fmt.Fprintf(stderr, "run-loop stopped\n")
			return nil
		case <-ticker.C:
			// Jittered backoff: add up to 10% jitter so multiple devices do
			// not stampede the hub on the same schedule.
			//nolint:gosec // jitter does not need cryptographic randomness.
			jitter := time.Duration(rand.Int64N(runLoopJitterBound(interval)))
			if jitter > 0 {
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(jitter):
				}
			}
			if err := tick(); err != nil {
				return err
			}
		}
	}
}
