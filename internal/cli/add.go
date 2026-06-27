package cli

import (
	"fmt"
	"io"
	"path/filepath"

	dsgit "github.com/Reederey87/DevStrap/internal/git"
	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
	"github.com/spf13/cobra"
)

func newAddCommand(stdout io.Writer, opts *options) *cobra.Command {
	var nsPath string
	var defaultBranch string
	var lfsPolicy string
	cmd := &cobra.Command{
		Use:   "add <remote>",
		Short: "Add a Git repository to the namespace",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if nsPath == "" {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("--path is required")}
			}
			remoteKey, err := dsgit.CanonicalRemoteKey(args[0])
			if err != nil {
				return appError{code: exitInvalidConfig, err: err}
			}
			if defaultBranch == "" {
				defaultBranch = "main"
			}
			if lfsPolicy == "" {
				lfsPolicy = "auto"
			}
			if !validLFSPolicy(lfsPolicy) {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("unsupported lfs policy %q", lfsPolicy)}
			}
			store, err := opts.openState()
			if err != nil {
				return err
			}
			defer closeStore(store)
			localPath := filepath.Join(opts.paths().Root, filepath.FromSlash(nsPath))
			if err := ensureHydratableTarget(localPath); err != nil {
				return err
			}
			event, err := dssync.CreateProjectEvent(cmd.Context(), store, dssync.EventProjectAdded, dssync.ProjectPayload{
				Path:          nsPath,
				Type:          "git_repo",
				RemoteURL:     args[0],
				RemoteKey:     remoteKey,
				DefaultBranch: defaultBranch,
			})
			if err != nil {
				return err
			}
			project, err := store.UpsertProject(cmd.Context(), state.UpsertProjectParams{
				Path:                  nsPath,
				Type:                  "git_repo",
				RemoteURL:             args[0],
				RemoteKey:             remoteKey,
				DefaultBranch:         defaultBranch,
				LFSPolicy:             lfsPolicy,
				MaterializationPolicy: "lazy",
				LocalPath:             localPath,
				MaterializationState:  "skeleton",
				DirtyState:            "unknown",
				SourceEventHLC:        event.HLC,
				SourceEventDeviceID:   event.DeviceID,
				SourceEventID:         event.ID,
			})
			if err != nil {
				return err
			}
			if err := writeSkeleton(localPath, project.Path, args[0]); err != nil {
				return err
			}
			_, err = fmt.Fprintf(stdout, "Added %s -> %s\n", project.Path, args[0])
			return err
		},
	}
	cmd.Flags().StringVar(&nsPath, "path", "", "namespace path")
	cmd.Flags().StringVar(&defaultBranch, "default-branch", "", "default branch fallback")
	cmd.Flags().StringVar(&lfsPolicy, "lfs-policy", "auto", "Git LFS policy for agent worktrees: auto, never, agent, or always")
	return cmd
}
