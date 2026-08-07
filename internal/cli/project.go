package cli

// devstrap project sparse set|list|clear <project> — cone-mode
// sparse-checkout profile management (W12-02). See internal/git/sparse.go
// for the git primitives and spec/08_GIT_MATERIALIZATION_AND_WORKTREES.md for
// the distinction between this (git's own working-tree cone, applies AFTER a
// repo is materialized) and .devstrapignore (controls sync/materialization
// inclusion universally, BEFORE anything is cloned). The profile is
// deliberately local-only — never synced through the event log — so `set`/
// `clear` immediately re-apply against an already-materialized checkout
// rather than waiting for the next sync.

import (
	"fmt"
	"io"
	"strings"

	dsgit "github.com/Reederey87/DevStrap/internal/git"
	"github.com/Reederey87/DevStrap/internal/redact"
	"github.com/spf13/cobra"
)

func newProjectCommand(stdout io.Writer, opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "Manage per-project local configuration",
	}
	cmd.AddCommand(newProjectSparseCommand(stdout, opts))
	return cmd
}

func newProjectSparseCommand(stdout io.Writer, opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sparse",
		Short: "Manage a project's cone-mode sparse-checkout profile",
	}
	cmd.AddCommand(newProjectSparseSetCommand(stdout, opts))
	cmd.AddCommand(newProjectSparseListCommand(stdout, opts))
	cmd.AddCommand(newProjectSparseClearCommand(stdout, opts))
	return cmd
}

type projectSparseResult struct {
	Path     string   `json:"path"`
	Paths    []string `json:"paths,omitempty"`
	Applied  bool     `json:"applied"`
	Warnings []string `json:"warnings,omitempty"`
}

// newProjectSparseSetCommand configures a project's cone-mode sparse-checkout
// directories and, for an already-materialized project, applies them
// immediately.
//
// Lock ordering (Codex review, W12-02): the project's repo lock is acquired
// ONCE, before the DB write, and held across BOTH the DB write and the git
// apply — matching every other mutating git operation in this codebase
// (hydrate, worktree new both hold the lock across their own state write and
// git work). The original version wrote the DB first and only acquired the
// lock inside a later helper for the git apply, which let a concurrent
// `sparse set`/`clear`/sync interleave DB writes and git applies out of
// order and leave the DB's desired state and the on-disk cone permanently
// mismatched (e.g. two concurrent `set`s could apply-then-write in an order
// that leaves the tree narrowed to the OTHER command's paths).
func newProjectSparseSetCommand(stdout io.Writer, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "set <path> <dir1> [dir2...]",
		Short: "Configure (and re-apply) a project's cone-mode sparse-checkout directories",
		Args:  usageArgs(cobra.MinimumNArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			paths, err := cleanSparseArgs(args[1:])
			if err != nil {
				return appError{code: exitInvalidConfig, err: err}
			}
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			project, err := store.ProjectByPath(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if project.Type != "git_repo" {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("%s is %s, not git_repo; sparse-checkout only applies to git repos", project.Path, project.Type)}
			}

			materialized := project.LocalPath != "" && dsgit.IsRepo(project.LocalPath)
			if materialized {
				unlock, lockErr := acquireRepoLock(opts.paths().Home, project.ID)
				if lockErr != nil {
					return lockErr
				}
				defer unlock()
			}

			if err := store.ReplaceSparsePathsForProject(cmd.Context(), project.ID, paths); err != nil {
				return err
			}

			applied := false
			var applyErr error
			if materialized {
				r := gitRunner(opts)
				if err := r.ApplyConvergedSparseCheckout(cmd.Context(), project.LocalPath, paths); err != nil {
					applyErr = err
				} else {
					applied = true
				}
			}
			var warnings []string
			if applyErr != nil {
				warnings = append(warnings, fmt.Sprintf("apply to the current checkout failed: %v; will retry on the next sync/hydrate", applyErr))
				_ = store.RecordProjectWarning(cmd.Context(), project.ID, redact.Scrub(fmt.Sprintf("sparse-checkout set: %v", applyErr)))
			}
			return opts.render(stdout, func(w io.Writer) error {
				switch {
				case applied:
					_, err := fmt.Fprintf(w, "Configured sparse-checkout for %s: %s (applied to the materialized checkout)\n", project.Path, strings.Join(paths, ", "))
					return err
				case applyErr != nil:
					_, err := fmt.Fprintf(w, "Configured sparse-checkout for %s: %s (apply to the current checkout failed: %v; will retry on the next sync/hydrate)\n", project.Path, strings.Join(paths, ", "), applyErr)
					return err
				default:
					_, err := fmt.Fprintf(w, "Configured sparse-checkout for %s: %s (will apply on the next sync/hydrate)\n", project.Path, strings.Join(paths, ", "))
					return err
				}
			}, projectSparseResult{Path: project.Path, Paths: paths, Applied: applied, Warnings: warnings})
		},
	}
}

func newProjectSparseListCommand(stdout io.Writer, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "list <path>",
		Short: "Show a project's configured cone-mode sparse-checkout directories",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			project, err := store.ProjectByPath(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			paths, err := store.SparsePathsForProject(cmd.Context(), project.ID)
			if err != nil {
				return err
			}
			return opts.render(stdout, func(w io.Writer) error {
				if len(paths) == 0 {
					_, err := fmt.Fprintf(w, "No sparse-checkout profile configured for %s (full working tree)\n", project.Path)
					return err
				}
				for _, p := range paths {
					if _, err := fmt.Fprintln(w, p); err != nil {
						return err
					}
				}
				return nil
			}, projectSparseResult{Path: project.Path, Paths: paths})
		},
	}
}

// newProjectSparseClearCommand removes a project's sparse-checkout profile.
//
// The DB write (the desired-state source of truth) always happens, even when
// the immediate git disable fails or the project isn't materialized yet
// (Codex review, W12-02): a failed disable here self-heals on the next
// sync/hydrate, because applyProjectSparseProfile (hydrate.go) now converges
// BIDIRECTIONALLY — an empty configured set also disables an on-disk cone
// left over from exactly this failure mode, so a `clear` that could not
// immediately apply is never stuck narrowed forever. The lock is acquired
// once, before touching git, and released before the DB write (SQLite has
// its own concurrency control; the repo lock only needs to cover the git
// mutation).
func newProjectSparseClearCommand(stdout io.Writer, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "clear <path>",
		Short: "Remove a project's sparse-checkout profile and restore a full working tree",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			project, err := store.ProjectByPath(cmd.Context(), args[0])
			if err != nil {
				return err
			}

			applied := false
			var applyErr error
			if project.LocalPath != "" && dsgit.IsRepo(project.LocalPath) {
				unlock, lockErr := acquireRepoLock(opts.paths().Home, project.ID)
				if lockErr != nil {
					applyErr = lockErr
				} else {
					r := gitRunner(opts)
					if err := r.SparseCheckoutDisable(cmd.Context(), project.LocalPath); err != nil {
						applyErr = err
					} else {
						applied = true
					}
					unlock()
				}
			}

			if err := store.ReplaceSparsePathsForProject(cmd.Context(), project.ID, nil); err != nil {
				return err
			}
			var warnings []string
			if applyErr != nil {
				warnings = append(warnings, fmt.Sprintf("restoring the full working tree failed: %v; will retry on the next sync/hydrate", applyErr))
				_ = store.RecordProjectWarning(cmd.Context(), project.ID, redact.Scrub(fmt.Sprintf("sparse-checkout clear: %v", applyErr)))
			}
			return opts.render(stdout, func(w io.Writer) error {
				switch {
				case applied:
					_, err := fmt.Fprintf(w, "Cleared sparse-checkout profile for %s (restored full working tree)\n", project.Path)
					return err
				case applyErr != nil:
					_, err := fmt.Fprintf(w, "Cleared sparse-checkout profile for %s (restoring the full working tree failed: %v; will retry on the next sync/hydrate)\n", project.Path, applyErr)
					return err
				default:
					_, err := fmt.Fprintf(w, "Cleared sparse-checkout profile for %s (will restore a full working tree on the next sync/hydrate)\n", project.Path)
					return err
				}
			}, projectSparseResult{Path: project.Path, Applied: applied, Warnings: warnings})
		},
	}
}

// cleanSparseArgs cleans and validates a list of positional directory
// arguments the same way parseSparseFlag does for the comma-separated `add
// --sparse` flag, de-duplicating while preserving order. The result is also
// normalized (dsgit.NormalizeSparsePaths, review follow-up) so an
// overlapping `project sparse set p src src/lib` stores just ["src"] rather
// than a pair that would permanently defeat convergence's no-op check — see
// parseSparseFlag's doc comment for the full rationale.
func cleanSparseArgs(raw []string) ([]string, error) {
	seen := make(map[string]bool, len(raw))
	paths := make([]string, 0, len(raw))
	for _, arg := range raw {
		p := dsgit.CleanSparsePath(arg)
		if p == "" {
			return nil, fmt.Errorf("sparse directory argument must not be empty")
		}
		if err := dsgit.ValidSparsePath(p); err != nil {
			return nil, err
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		paths = append(paths, p)
	}
	return dsgit.NormalizeSparsePaths(paths), nil
}
