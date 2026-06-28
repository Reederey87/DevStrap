package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

func newConflictsCommand(stdout io.Writer, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "conflicts",
		Short: "List open namespace conflicts (PROD-02)",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			conflicts, err := store.OpenConflicts(cmd.Context())
			if err != nil {
				return err
			}
			if opts.v.GetBool("json") {
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(conflicts)
			}
			if len(conflicts) == 0 {
				_, err = fmt.Fprintln(stdout, "No open conflicts.")
				return err
			}
			_, _ = fmt.Fprintf(stdout, "%d open conflict(s):\n\n", len(conflicts))
			for _, c := range conflicts {
				_, _ = fmt.Fprintf(stdout, "ID: %s\nType: %s\n", c.ID, c.Type)
				if c.NamespaceID != "" {
					_, _ = fmt.Fprintf(stdout, "Project: %s\n", c.NamespaceID)
				}
				_, _ = fmt.Fprintf(stdout, "Details: %s\n\n", c.DetailsJSON)
			}
			_, _ = fmt.Fprintln(stdout, "Resolve the underlying issue and re-run the originating command.")
			return nil
		},
	}
}
