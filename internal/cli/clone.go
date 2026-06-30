package cli

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	dsgit "github.com/Reederey87/DevStrap/internal/git"
	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/spf13/cobra"
)

// newCloneCommand implements `devstrap clone <url> [path]` (PROD-01): a one-shot
// quick path that collapses onboarding (add -> eager materialize -> optional
// open) into a single command. It derives a namespace path from the remote when
// one is not given, reuses the existing addProject + materializeOne internals,
// and prints the resulting path so time-to-first-success is one command.
func newCloneCommand(stdout io.Writer, opts *options) *cobra.Command {
	var openCursor bool
	var openVSCode bool
	var defaultBranch string
	var lfsPolicy string
	cmd := &cobra.Command{
		Use:   "clone <url> [path]",
		Short: "Clone a Git repo into the namespace and materialize it in one command",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			remote := args[0]
			nsPath := ""
			if len(args) == 2 {
				nsPath = args[1]
			} else {
				derived, err := deriveClonePath(remote)
				if err != nil {
					return appError{code: exitInvalidConfig, err: fmt.Errorf("derive namespace path: %w", err)}
				}
				nsPath = derived
			}
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			project, err := addProject(cmd.Context(), store, opts, remote, nsPath, defaultBranch, lfsPolicy)
			if err != nil {
				return appError{code: exitInvalidConfig, err: err}
			}
			if _, err := fmt.Fprintf(stdout, "added %s -> %s\n", project.Path, remote); err != nil {
				return err
			}
			// Fetch the full ProjectStatus (with LocalPath/materialization
			// state) for the materialization pass; UpsertProject returns the
			// NamespaceEntry, but materializeOne needs ProjectStatus.
			status, err := store.ProjectByPath(cmd.Context(), project.Path)
			if err != nil {
				return err
			}
			// Eager materialization: blobless clone + env hydrate (EAGER-01/03).
			if err := materializeOne(cmd.Context(), store, opts, status); err != nil {
				return appError{code: exitGit, err: fmt.Errorf("clone %s: %w", project.Path, err)}
			}
			if _, err := fmt.Fprintf(stdout, "cloned %s\n", project.Path); err != nil {
				return err
			}
			if openCursor || openVSCode {
				if openCursor && openVSCode {
					return appError{code: exitInvalidConfig, err: fmt.Errorf("choose --open or --vscode, not both")}
				}
				editor := "cursor"
				if openVSCode {
					editor = "code"
				}
				localPath := status.LocalPath
				if localPath == "" {
					localPath = filepath.Join(opts.paths().Root, filepath.FromSlash(project.Path))
				}
				if err := platform.Detect().Editor.Open(cmd.Context(), localPath, editor); err != nil {
					if errors.Is(err, platform.ErrEditorNotFound) {
						return appError{code: exitInvalidConfig, err: fmt.Errorf("%s command not found", editor)}
					}
					return err
				}
				if _, err := fmt.Fprintf(stdout, "opened %s with %s\n", localPath, editor); err != nil {
					return err
				}
			} else {
				if _, err := fmt.Fprintf(stdout, "Open with: devstrap open %s --cursor\n", project.Path); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&openCursor, "open", false, "open the cloned repo in Cursor after materialization")
	cmd.Flags().BoolVar(&openVSCode, "vscode", false, "open the cloned repo in VS Code after materialization")
	cmd.Flags().StringVar(&defaultBranch, "default-branch", "", "default branch fallback")
	cmd.Flags().StringVar(&lfsPolicy, "lfs-policy", "auto", "Git LFS policy: auto, never, agent, or always")
	return cmd
}

// deriveClonePath derives a namespace path from a git remote URL: work/<org>/<repo>.
// The canonical remote key is host/org/repo; the host is stripped so the path
// is portable across SSH/HTTPS forms of the same repo.
func deriveClonePath(remote string) (string, error) {
	key, err := dsgit.CanonicalRemoteKey(remote)
	if err != nil {
		return "", err
	}
	parts := strings.SplitN(key, "/", 2)
	if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
		return "", fmt.Errorf("cannot derive namespace path from remote %q", remote)
	}
	return "work/" + parts[1], nil
}
