package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/cobra"
)

func newStatusCommand(stdout io.Writer, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show local workspace status",
		RunE: func(cmd *cobra.Command, args []string) error {
			// A missing state database means the workspace was never
			// initialized; surface the friendly guidance instead of a raw
			// sqlite "unable to open database file" error.
			if _, err := os.Stat(opts.paths().StateDB()); errors.Is(err, os.ErrNotExist) {
				return appError{code: exitInvalidConfig, err: state.ErrNotInitialized}
			}
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)

			summary, err := store.Summary(cmd.Context())
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

			_, err = fmt.Fprintf(stdout, "Workspace: %s\nRoot: %s\nProjects: %d\n", summary.WorkspaceName, summary.RootPath, summary.ProjectCount)
			if err != nil {
				return err
			}
			// PROD-02: surface open conflict count in status.
			if n, err := store.CountOpenConflicts(cmd.Context()); err == nil && n > 0 {
				_, _ = fmt.Fprintf(stdout, "Open conflicts: %d (run `devstrap conflicts` to inspect)\n", n)
			}
			if len(summary.Projects) > 0 {
				_, _ = fmt.Fprintln(stdout, "\nProject\tType\tStatus\tDirty")
				for _, project := range summary.Projects {
					// PROD-01: derive a display status from the materialization
					// and dirty states instead of showing raw values.
					status := deriveDisplayStatus(project.MaterializationState, project.DirtyState)
					dirty := project.DirtyState
					if dirty == "" {
						dirty = "unknown"
					}
					_, _ = fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", project.Path, project.Type, status, dirty)
				}
			}
			return err
		},
	}
}

// deriveDisplayStatus maps the raw materialization and dirty states to a
// user-facing display label (PROD-01). When env_ready/tooling_ready are
// implemented, this function should expand to incorporate them.
func deriveDisplayStatus(materialization, dirty string) string {
	switch {
	case materialization == "failed":
		return "failed"
	case materialization == "skeleton":
		return "skeleton"
	case materialization == "hydrating":
		return "hydrating"
	case materialization == "materialized-empty":
		return "empty checkout"
	case dirty == "dirty" || dirty == "diverged":
		return "dirty"
	case dirty == "clean" && materialization == "hydrated":
		return "ready"
	case dirty == "clean":
		return "current"
	case materialization == "hydrated":
		return "ready"
	default:
		return "available"
	}
}
