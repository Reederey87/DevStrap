package cli

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"time"

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
			if _, _, err := hubFromOptions(opts, hubFile); err != nil {
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
	_, _ = fmt.Fprintf(stderr, "[%s] run-loop tick: sync + materialize\n", time.Now().UTC().Format(time.RFC3339))
	return runSyncCycle(ctx, stdout, opts, hubFile, namespaceOnly, false)
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
