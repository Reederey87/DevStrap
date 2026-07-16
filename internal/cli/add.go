package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	dsgit "github.com/Reederey87/DevStrap/internal/git"
	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
	"github.com/spf13/cobra"
)

// addResult is the --json shape for `devstrap add` (P5-CLI-01 part B).
type addResult struct {
	Path   string `json:"path"`
	Remote string `json:"remote"`
}

func newAddCommand(stdout io.Writer, opts *options) *cobra.Command {
	var nsPath string
	var defaultBranch string
	var lfsPolicy string
	cmd := &cobra.Command{
		Use:   "add <remote>",
		Short: "Add a Git repository to the namespace",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if nsPath == "" {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("--path is required")}
			}
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			project, err := addProject(cmd.Context(), store, opts, args[0], nsPath, defaultBranch, lfsPolicy)
			if err != nil {
				return appError{code: exitInvalidConfig, err: err}
			}
			return opts.render(stdout, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "Added %s -> %s\n", project.Path, args[0])
				return err
			}, addResult{Path: project.Path, Remote: args[0]})
		},
	}
	cmd.Flags().StringVar(&nsPath, "path", "", "namespace path")
	cmd.Flags().StringVar(&defaultBranch, "default-branch", "", "default branch fallback")
	cmd.Flags().StringVar(&lfsPolicy, "lfs-policy", "auto", "Git LFS policy for agent worktrees: auto, never, agent, or always")
	return cmd
}

// addProject validates the remote, creates a project.added event, upserts the
// namespace entry, and writes the skeleton directory. Shared by `devstrap add`
// and `devstrap clone` (PROD-01) so clone is a thin orchestrator over the
// existing add path rather than new core logic.
func addProject(ctx context.Context, store *state.Store, opts *options, remote, nsPath, defaultBranch, lfsPolicy string) (state.NamespaceEntry, error) {
	remoteKey, err := dsgit.CanonicalRemoteKey(remote)
	if err != nil {
		return state.NamespaceEntry{}, err
	}
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	if lfsPolicy == "" {
		lfsPolicy = "auto"
	}
	if !validLFSPolicy(lfsPolicy) {
		return state.NamespaceEntry{}, fmt.Errorf("unsupported lfs policy %q", lfsPolicy)
	}
	localPath := filepath.Join(opts.paths().Root, filepath.FromSlash(nsPath))
	if err := ensureHydratableTarget(localPath); err != nil {
		return state.NamespaceEntry{}, err
	}
	var project state.NamespaceEntry
	if err := store.WithTx(ctx, func(tx *state.Tx) error {
		event, err := dssync.CreateProjectEventTx(ctx, store, tx, dssync.EventProjectAdded, dssync.ProjectPayload{
			Path:          nsPath,
			Type:          "git_repo",
			RemoteURL:     remote,
			RemoteKey:     remoteKey,
			DefaultBranch: defaultBranch,
		})
		if err != nil {
			return err
		}
		project, err = tx.UpsertProject(ctx, state.UpsertProjectParams{
			Path:                  nsPath,
			Type:                  "git_repo",
			RemoteURL:             remote,
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
		return err
	}); err != nil {
		return state.NamespaceEntry{}, err
	}
	if err := writeSkeleton(localPath, project.Path, remote); err != nil {
		return state.NamespaceEntry{}, err
	}
	return project, nil
}
