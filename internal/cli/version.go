package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// versionResult is the --json shape for `devstrap version` (P5-CLI-01 part B).
type versionResult struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

func newVersionCommand(stdout io.Writer, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print build version",
		RunE: func(cmd *cobra.Command, args []string) error {
			return opts.render(stdout, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "devstrap %s (%s, %s)\n", version, commit, date)
				return err
			}, versionResult{Version: version, Commit: commit, Date: date})
		},
	}
}
