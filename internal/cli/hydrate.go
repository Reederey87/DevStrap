package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	dsgit "github.com/Reederey87/DevStrap/internal/git"
	"github.com/Reederey87/DevStrap/internal/pathkey"
	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/cobra"
)

func newHydrateCommand(stdout io.Writer, opts *options) *cobra.Command {
	var partial bool
	var full bool
	var lfs bool
	cmd := &cobra.Command{
		Use:   "hydrate <path>",
		Short: "Clone a skeleton Git repository",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if partial && full {
				return appError{code: exitInvalidConfig, err: errors.New("use either --partial or --full")}
			}
			localPath, err := hydrateProject(cmd.Context(), opts, args[0], !full)
			if err != nil {
				return err
			}
			if lfs {
				r := dsgit.NewRunner()
				if _, err := r.Run(cmd.Context(), localPath, "lfs", "pull"); err != nil {
					return err
				}
			}
			_, err = fmt.Fprintf(stdout, "Hydrated %s\n", localPath)
			return err
		},
	}
	cmd.Flags().BoolVar(&partial, "partial", true, "use partial clone with blob filtering")
	cmd.Flags().BoolVar(&full, "full", false, "use a full clone")
	cmd.Flags().BoolVar(&lfs, "lfs", false, "pull Git LFS objects after clone")
	return cmd
}

func hydrateProject(ctx context.Context, opts *options, nsPath string, partial bool) (string, error) {
	store, err := opts.openState(ctx)
	if err != nil {
		return "", err
	}
	defer closeStore(store)
	project, err := store.ProjectByPath(ctx, nsPath)
	if err != nil {
		return "", err
	}
	unlock, err := acquireRepoLock(opts.paths().Home, project.ID)
	if err != nil {
		return "", err
	}
	defer unlock()
	return hydrateProjectUnlocked(ctx, store, opts, project, partial)
}

func hydrateProjectUnlocked(ctx context.Context, store *state.Store, opts *options, project state.ProjectStatus, partial bool) (string, error) {
	if project.Type != "git_repo" {
		return "", appError{code: exitInvalidConfig, err: fmt.Errorf("%s is %s, not git_repo", project.Path, project.Type)}
	}
	localPath := project.LocalPath
	if localPath == "" {
		localPath = filepath.Join(opts.paths().Root, filepath.FromSlash(project.Path))
	}
	// SEC-4: re-validate at use time that the materialization target still
	// resolves within the managed root. This closes the TOCTOU window where a
	// symlink in the path was repointed outside the root after scan time.
	if root := opts.paths().Root; root != "" {
		if err := pathkey.VerifyWithinRoot(root, localPath); err != nil {
			return "", appError{code: exitInvalidConfig, err: fmt.Errorf("refusing to materialize outside managed root: %w", err)}
		}
	}
	if dsgit.IsRepo(localPath) {
		r := dsgit.NewRunner()
		dirty, _ := r.DirtyState(ctx, localPath)
		_ = store.UpdateProjectLocalState(ctx, project.ID, localPath, "available", string(dirty))
		return localPath, nil
	}
	if err := ensureHydratableTarget(localPath); err != nil {
		return "", err
	}
	tmpPath, err := cloneTempDir(localPath)
	if err != nil {
		return "", err
	}
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = os.RemoveAll(tmpPath)
		}
	}()
	r := dsgit.NewRunner()
	if err := r.Clone(ctx, project.RemoteURL, tmpPath, partial); err != nil {
		_ = store.UpdateProjectLocalState(ctx, project.ID, localPath, "failed", "unknown")
		return "", err
	}
	if err := promoteClonedRepo(tmpPath, localPath, project.Path, project.RemoteURL); err != nil {
		_ = store.UpdateProjectLocalState(ctx, project.ID, localPath, "failed", "unknown")
		return "", err
	}
	cleanupTmp = false
	dirty, _ := r.DirtyState(ctx, localPath)
	if err := store.UpdateProjectLocalState(ctx, project.ID, localPath, "available", string(dirty)); err != nil {
		return "", err
	}
	return localPath, nil
}

func cloneTempDir(targetPath string) (string, error) {
	parent := filepath.Dir(targetPath)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return "", fmt.Errorf("create clone parent: %w", err)
	}
	tmpPath, err := os.MkdirTemp(parent, "."+filepath.Base(targetPath)+".devstrap-tmp-*")
	if err != nil {
		return "", fmt.Errorf("create clone temp dir: %w", err)
	}
	return tmpPath, nil
}

func promoteClonedRepo(tmpPath, targetPath, nsPath, remote string) error {
	targetWasSkeleton := isSkeleton(targetPath)
	if err := ensureHydratableTarget(targetPath); err != nil {
		return err
	}
	if _, err := os.Stat(targetPath); err == nil {
		if err := os.RemoveAll(targetPath); err != nil {
			return fmt.Errorf("remove hydratable target: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat hydratable target: %w", err)
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		if targetWasSkeleton {
			_ = writeSkeleton(targetPath, nsPath, remote)
		}
		return fmt.Errorf("promote clone: %w", err)
	}
	return nil
}

func ensureHydratableTarget(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
				return fmt.Errorf("create parent directory: %w", err)
			}
			return nil
		}
		return fmt.Errorf("read target directory: %w", err)
	}
	if len(entries) == 0 || isSkeleton(path) {
		return nil
	}
	return appError{code: exitDirtyWorktree, err: fmt.Errorf("refusing to hydrate into non-empty directory: %s", path)}
}

func writeSkeleton(path, nsPath, remote string) error {
	if dsgit.IsRepo(path) {
		return nil
	}
	//nolint:gosec // Skeleton metadata lives inside the user's managed code tree and should be readable as project files.
	if err := os.MkdirAll(filepath.Join(path, ".devstrap"), 0o755); err != nil {
		return fmt.Errorf("create skeleton: %w", err)
	}
	placeholder := fmt.Sprintf("{\n  \"path\": %q,\n  \"remote\": %q,\n  \"state\": \"skeleton\"\n}\n", nsPath, remote)
	//nolint:gosec // Skeleton metadata is non-secret project state.
	if err := os.WriteFile(filepath.Join(path, ".devstrap", "placeholder.json"), []byte(placeholder), 0o644); err != nil {
		return fmt.Errorf("write placeholder: %w", err)
	}
	readme := fmt.Sprintf("# DevStrap skeleton\n\nThis directory maps to `%s` and will be hydrated from `%s`.\n", nsPath, remote)
	//nolint:gosec // Skeleton README is non-secret project documentation.
	if err := os.WriteFile(filepath.Join(path, "README.devstrap.md"), []byte(readme), 0o644); err != nil {
		return fmt.Errorf("write skeleton readme: %w", err)
	}
	return nil
}

func isSkeleton(path string) bool {
	if _, err := os.Stat(filepath.Join(path, ".devstrap", "placeholder.json")); err != nil {
		return false
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.Name() != ".devstrap" && entry.Name() != "README.devstrap.md" {
			return false
		}
	}
	return true
}
