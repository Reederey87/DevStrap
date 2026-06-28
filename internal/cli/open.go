package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/spf13/cobra"
)

func newOpenCommand(stdout io.Writer, opts *options) *cobra.Command {
	var cursor bool
	var vscode bool
	cmd := &cobra.Command{
		Use:   "open <path>",
		Short: "Hydrate and open a namespace path in an editor",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cursor == vscode {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("choose exactly one of --cursor or --vscode")}
			}
			localPath, err := hydrateProject(cmd.Context(), opts, args[0], true)
			if err != nil {
				return err
			}
			editor := "cursor"
			if vscode {
				editor = "code"
			}
			if err := platform.Detect().Editor.Open(cmd.Context(), localPath, editor); err != nil {
				if errors.Is(err, platform.ErrEditorNotFound) {
					return appError{code: exitInvalidConfig, err: fmt.Errorf("%s command not found", editor)}
				}
				return err
			}
			_, err = fmt.Fprintf(stdout, "Opened %s with %s\n", localPath, editor)
			return err
		},
	}
	cmd.Flags().BoolVar(&cursor, "cursor", false, "open with Cursor")
	cmd.Flags().BoolVar(&vscode, "vscode", false, "open with VS Code")
	return cmd
}
