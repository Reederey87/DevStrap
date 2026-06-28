package cli

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"time"

	"github.com/spf13/cobra"
)

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
			if hubFile == "" {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("--hub-file is required until the production hub exists")}
			}
			if once {
				return runLoopTick(cmd.Context(), stdout, opts, hubFile, namespaceOnly)
			}
			// XP-02: foreground loop with jittered backoff. Graceful shutdown
			// on context cancellation (Ctrl-C / scheduler stop).
			return runLoopForever(cmd.Context(), stdout, opts, hubFile, namespaceOnly, interval)
		},
	}
	cmd.Flags().StringVar(&hubFile, "hub-file", "", "file-backed test hub path")
	cmd.Flags().DurationVar(&interval, "interval", 5*time.Minute, "time between sync cycles")
	cmd.Flags().BoolVar(&once, "once", false, "run a single sync cycle and exit (for cron)")
	cmd.Flags().BoolVar(&namespaceOnly, "namespace-only", false, "sync namespace metadata only; skip materialization")
	return cmd
}

func runLoopTick(ctx context.Context, stdout io.Writer, opts *options, hubFile string, namespaceOnly bool) error {
	_, _ = fmt.Fprintf(stdout, "[%s] run-loop tick: sync + materialize\n", time.Now().UTC().Format(time.RFC3339))
	return runSyncCycle(ctx, stdout, opts, hubFile, namespaceOnly, false)
}

func runLoopForever(ctx context.Context, stdout io.Writer, opts *options, hubFile string, namespaceOnly bool, interval time.Duration) error {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	// Run one tick immediately on startup, then on the interval.
	if err := runLoopTick(ctx, stdout, opts, hubFile, namespaceOnly); err != nil {
		_, _ = fmt.Fprintf(stdout, "run-loop tick error: %v\n", err)
	}
	for {
		select {
		case <-ctx.Done():
			_, _ = fmt.Fprintf(stdout, "run-loop stopped\n")
			return nil
		case <-ticker.C:
			// Jittered backoff: add up to 10% jitter so multiple devices do
			// not stampede the hub on the same schedule.
			//nolint:gosec // jitter does not need cryptographic randomness.
			jitter := time.Duration(rand.Int64N(int64(interval) / 10))
			if jitter > 0 {
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(jitter):
				}
			}
			if err := runLoopTick(ctx, stdout, opts, hubFile, namespaceOnly); err != nil {
				_, _ = fmt.Fprintf(stdout, "run-loop tick error: %v\n", err)
			}
		}
	}
}
