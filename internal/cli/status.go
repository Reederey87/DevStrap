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
			store, err := opts.openState()
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
			if len(summary.Projects) > 0 {
				_, _ = fmt.Fprintln(stdout, "\nProject\tType\tCode\tDirty")
				for _, project := range summary.Projects {
					code := project.MaterializationState
					if code == "" {
						code = "unknown"
					}
					dirty := project.DirtyState
					if dirty == "" {
						dirty = "unknown"
					}
					_, _ = fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", project.Path, project.Type, code, dirty)
				}
			}
			return err
		},
	}
}
