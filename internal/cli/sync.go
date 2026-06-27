package cli

import (
	"errors"
	"fmt"
	"io"

	dssync "github.com/Reederey87/DevStrap/internal/sync"
	"github.com/spf13/cobra"
)

func newSyncCommand(stdout io.Writer, opts *options) *cobra.Command {
	var hubFile string
	var namespaceOnly bool
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Push and pull namespace events",
		RunE: func(cmd *cobra.Command, args []string) error {
			if hubFile == "" {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("--hub-file is required until the production hub exists")}
			}
			store, err := opts.openState()
			if err != nil {
				return err
			}
			defer closeStore(store)
			localEvents, err := store.PendingEvents(cmd.Context())
			if err != nil {
				return err
			}
			if dryRun {
				_, err = fmt.Fprintf(stdout, "Would push %d local events to %s and pull namespace events\n", len(localEvents), hubFile)
				return err
			}
			hub := dssync.FileHub{Path: hubFile}
			if err := hub.Push(cmd.Context(), localEvents); err != nil {
				return appError{code: exitNetwork, err: err}
			}
			remoteEvents, err := hub.Pull(cmd.Context(), 0)
			if err != nil {
				if errors.Is(err, dssync.ErrSnapshotRequired) {
					return appError{code: exitNetwork, err: err}
				}
				return err
			}
			if err := dssync.ApplyEvents(cmd.Context(), store, remoteEvents); err != nil {
				return err
			}
			if namespaceOnly {
				_, err = fmt.Fprintf(stdout, "Synced namespace events: pushed %d, pulled %d\n", len(localEvents), len(remoteEvents))
				return err
			}
			_, err = fmt.Fprintf(stdout, "Synced events: pushed %d, pulled %d; hydration/fetch reconciliation is not implemented yet\n", len(localEvents), len(remoteEvents))
			return err
		},
	}
	cmd.Flags().StringVar(&hubFile, "hub-file", "", "file-backed test hub path")
	cmd.Flags().BoolVar(&namespaceOnly, "namespace-only", false, "sync namespace metadata only")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show sync plan without writing")
	return cmd
}
