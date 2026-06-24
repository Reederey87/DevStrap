package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/Reederey87/DevStrap/internal/config"
	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func newInitCommand(stdout io.Writer) *cobra.Command {
	var workspaceName string
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "init [root]",
		Short: "Initialize a DevStrap workspace",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			paths := config.Paths{
				Home: viper.GetString("home"),
				Root: viper.GetString("root"),
			}
			if len(args) == 1 {
				paths.Root = args[0]
			}
			if workspaceName == "" {
				workspaceName = "default"
			}

			if dryRun {
				_, err := fmt.Fprintf(stdout, "Would create %s, %s, and %s\n", paths.Root, paths.Home, paths.StateDB())
				return err
			}

			if err := os.MkdirAll(paths.Root, 0o755); err != nil {
				return fmt.Errorf("create root: %w", err)
			}
			if err := os.MkdirAll(paths.Home, 0o700); err != nil {
				return fmt.Errorf("create state home: %w", err)
			}
			if err := os.MkdirAll(paths.LogDir(), 0o700); err != nil {
				return fmt.Errorf("create log dir: %w", err)
			}

			store, err := state.Open(paths.StateDB())
			if err != nil {
				return err
			}
			defer store.Close()

			if err := store.Migrate(); err != nil {
				return err
			}
			if err := store.EnsureWorkspace(cmd.Context(), workspaceName, paths.Root); err != nil {
				return err
			}

			_, err = fmt.Fprintf(stdout, "Initialized DevStrap workspace %q at %s\n", workspaceName, paths.Root)
			return err
		},
	}

	cmd.Flags().StringVar(&workspaceName, "workspace-name", "", "workspace name")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show planned changes without writing")
	return cmd
}
