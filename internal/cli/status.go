package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/cobra"
)

func newStatusCommand(stdout io.Writer, opts *options) *cobra.Command {
	var watch bool
	var interval time.Duration
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show local workspace status",
		RunE: func(cmd *cobra.Command, args []string) error {
			// A missing state database means the workspace was never
			// initialized; surface the friendly guidance instead of a raw
			// sqlite "unable to open database file" error.
			if _, err := os.Stat(opts.paths().StateDB()); errors.Is(err, os.ErrNotExist) {
				return appError{code: exitInvalidConfig, err: state.ErrNotInitialized}
			}
			if !watch {
				return renderStatus(cmd.Context(), stdout, opts)
			}
			// P5-PROD-05: live convergence view — re-render the readiness table,
			// open conflicts, and worktree/dirty state on an interval until the
			// user interrupts (Ctrl-C) or the scheduler stops the command.
			if interval <= 0 {
				interval = 2 * time.Second
			}
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			json := opts.v.GetBool("json")
			for {
				if !json {
					_, _ = fmt.Fprint(stdout, "\033[H\033[2J") // clear screen
				}
				if err := renderStatus(cmd.Context(), stdout, opts); err != nil {
					return err
				}
				if !json {
					_, _ = fmt.Fprintf(stdout, "\n(watching every %s — Ctrl-C to stop)\n", interval)
				}
				select {
				case <-cmd.Context().Done():
					return nil
				case <-ticker.C:
				}
			}
		},
	}
	cmd.Flags().BoolVar(&watch, "watch", false, "re-render status on an interval until interrupted (live convergence view)")
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "refresh interval for --watch")
	return cmd
}

// renderStatus renders a single status snapshot (PROD-01/02), honoring --json.
func renderStatus(ctx context.Context, stdout io.Writer, opts *options) error {
	store, err := opts.openState(ctx)
	if err != nil {
		return err
	}
	defer closeStore(store)

	summary, err := store.Summary(ctx)
	if err != nil {
		if errors.Is(err, state.ErrNotInitialized) {
			return appError{code: exitInvalidConfig, err: err}
		}
		return err
	}

	if opts.v.GetBool("json") {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(summary)
	}

	if _, err := fmt.Fprintf(stdout, "Workspace: %s\nRoot: %s\nProjects: %d\n", summary.WorkspaceName, summary.RootPath, summary.ProjectCount); err != nil {
		return err
	}
	// PROD-02: surface open conflict count in status.
	if n, err := store.CountOpenConflicts(ctx); err == nil && n > 0 {
		_, _ = fmt.Fprintf(stdout, "Open conflicts: %d (run `devstrap conflicts` to inspect)\n", n)
	}
	if len(summary.Projects) > 0 {
		_, _ = fmt.Fprintln(stdout, "\nProject\tType\tStatus\tDirty")
		for _, project := range summary.Projects {
			// PROD-01: derive a display status from the materialization and
			// dirty states instead of showing raw values.
			status := deriveDisplayStatus(project.MaterializationState, project.DirtyState)
			dirty := project.DirtyState
			if dirty == "" {
				dirty = "unknown"
			}
			_, _ = fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", project.Path, project.Type, status, dirty)
		}
	}
	return nil
}

// deriveDisplayStatus maps the raw materialization and dirty states to a
// user-facing display label (PROD-01 / P5-PROD-01). It branches ONLY on the
// states writers actually produce — "skeleton", "available", "failed",
// "materialized-empty" — so the headline "ready" state (a materialized,
// clean checkout) is reachable. The earlier "hydrated"/"hydrating" branches
// were dead (no writer ever set those values). When env_ready/tooling_ready
// land, expand "ready" to require them too.
func deriveDisplayStatus(materialization, dirty string) string {
	switch materialization {
	case "failed":
		return "failed"
	case "skeleton":
		return "skeleton"
	case "materialized-empty":
		return "empty checkout"
	}
	// materialization == "available" (or any materialized value writers emit):
	// distinguish a clean checkout ("ready") from a dirty one.
	switch dirty {
	case "dirty", "diverged":
		return "dirty"
	case "clean":
		return "ready"
	default:
		return "available"
	}
}
