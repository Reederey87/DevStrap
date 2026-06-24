package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func newStatusCommand(stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show local workspace status",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openState()
			if err != nil {
				return err
			}
			defer store.Close()

			summary, err := store.Summary(cmd.Context())
			if err != nil {
				return err
			}

			if viper.GetBool("json") {
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(summary)
			}

			_, err = fmt.Fprintf(stdout, "Workspace: %s\nRoot: %s\nProjects: %d\n", summary.WorkspaceName, summary.RootPath, summary.ProjectCount)
			return err
		},
	}
}
