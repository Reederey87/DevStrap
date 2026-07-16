package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	dsgit "github.com/Reederey87/DevStrap/internal/git"
	"github.com/Reederey87/DevStrap/internal/logging"
	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/cobra"
)

func newWorktreeCommand(stdout io.Writer, opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "worktree",
		Short: "Manage isolated worktrees",
	}
	cmd.AddCommand(newWorktreeNewCommand(stdout, opts))
	cmd.AddCommand(newWorktreeStatusCommand(stdout, opts))
	cmd.AddCommand(newWorktreeFinalizeCommand(stdout, opts))
	cmd.AddCommand(newWorktreeListCommand(stdout, opts))
	cmd.AddCommand(newWorktreeRemoveCommand(stdout, opts))
	cmd.AddCommand(newWorktreeCleanupCommand(stdout, opts))
	cmd.AddCommand(newWorktreeUnlockCommand(stdout, opts))
	return cmd
}

type repoLockReport struct {
	ProjectID string `json:"project_id"`
	Held      bool   `json:"held"`
	Stale     bool   `json:"stale"`
	Cleared   bool   `json:"cleared"`
	PID       int    `json:"pid,omitempty"`
	Hostname  string `json:"hostname,omitempty"`
	Acquired  string `json:"acquired_at,omitempty"`
}

func newWorktreeUnlockCommand(stdout io.Writer, opts *options) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "unlock <path>",
		Short: "Report and clear a stale repo operation lock for a project",
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
			home := opts.paths().Home
			info, held, stale, err := readRepoLock(home, project.ID)
			if err != nil {
				return err
			}
			report := repoLockReport{ProjectID: project.ID, Held: held, Stale: stale, PID: info.PID, Hostname: info.Hostname, Acquired: info.AcquiredAt}
			if held {
				cleared, err := clearRepoLock(home, project.ID, force)
				if err != nil {
					return err
				}
				report.Cleared = cleared
			}
			return opts.render(stdout, func(w io.Writer) error {
				switch {
				case !held:
					_, err = fmt.Fprintf(w, "No repo lock held for %s\n", project.Path)
				case report.Cleared:
					_, err = fmt.Fprintf(w, "Cleared %s repo lock for %s (pid %d on %s, acquired %s)\n", staleLabel(stale), project.Path, info.PID, info.Hostname, info.AcquiredAt)
				default:
					_, err = fmt.Fprintf(w, "Repo lock for %s held by pid %d on %s (acquired %s)\n", project.Path, info.PID, info.Hostname, info.AcquiredAt)
				}
				return err
			}, report)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "clear the lock even if its holder appears alive")
	return cmd
}

func staleLabel(stale bool) string {
	if stale {
		return "stale"
	}
	return "live"
}

func newWorktreeNewCommand(stdout io.Writer, opts *options) *cobra.Command {
	var freshUpstream bool
	var taskName string
	cmd := &cobra.Command{
		Use:   "new <path>",
		Short: "Create a fresh worktree from remote upstream",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !freshUpstream {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("--fresh-upstream is required")}
			}
			if taskName == "" {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("--name is required")}
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
			wt, err := createFreshWorktree(cmd.Context(), stdout, opts, store, project, taskName, "agent")
			if err != nil {
				return err
			}
			return opts.render(stdout, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "Created worktree %s at %s from %s %s\n", wt.Branch, wt.Path, wt.BaseRef, wt.BaseSHA)
				return err
			}, wt)
		},
	}
	cmd.Flags().BoolVar(&freshUpstream, "fresh-upstream", false, "base the worktree on fetched remote default branch")
	cmd.Flags().StringVar(&taskName, "name", "", "task name for branch slug")
	return cmd
}

func createFreshWorktree(ctx context.Context, stdout io.Writer, opts *options, store *state.Store, project state.ProjectStatus, taskName, createdBy string) (state.Worktree, error) {
	unlock, err := acquireRepoLock(opts.paths().Home, project.ID)
	if err != nil {
		return state.Worktree{}, err
	}
	defer unlock()
	return createFreshWorktreeLocked(ctx, stdout, opts, store, project, taskName, createdBy)
}

// createFreshWorktreeLocked is createFreshWorktree for callers that already
// hold the project repo lock — `agent run` keeps it held until the running
// agent_runs row exists, so `worktree cleanup` can never observe the fresh
// worktree without its run row (P7-GIT-01 startup window).
func createFreshWorktreeLocked(ctx context.Context, stdout io.Writer, opts *options, store *state.Store, project state.ProjectStatus, taskName, createdBy string) (state.Worktree, error) {
	// NOVCS-04: preflight — a remote-less repo cannot produce a fresh-upstream
	// worktree; fail fast with an actionable message before touching git.
	if strings.TrimSpace(project.RemoteKey) == "" {
		return state.Worktree{}, appError{code: exitInvalidConfig, err: fmt.Errorf("%s has no git remote; fresh-upstream worktrees require one (add one with 'git remote add origin <url>')", project.Path)}
	}
	localPath, err := hydrateProjectUnlocked(ctx, store, opts, project, true)
	if err != nil {
		return state.Worktree{}, err
	}
	r := gitRunner(opts)
	defaultBranch, err := resolveWorktreeDefaultBranch(ctx, stdout, r, localPath, project.DefaultBranch)
	if err != nil {
		return state.Worktree{}, appError{code: exitGit, err: err}
	}
	if err := r.Fetch(ctx, localPath, "origin", defaultBranch); err != nil {
		return state.Worktree{}, err
	}
	baseRef := "origin/" + defaultBranch
	baseSHA, err := r.RevParse(ctx, localPath, baseRef)
	if err != nil {
		return state.Worktree{}, err
	}
	if err := store.UpdateGitDefaultBranch(ctx, project.ID, defaultBranch); err != nil {
		return state.Worktree{}, err
	}
	branch, wtPath, err := addWorktreeWithFreshBranch(ctx, r, opts.paths().Home, project.ID, localPath, slugify(taskName), baseSHA)
	if err != nil {
		return state.Worktree{}, err
	}
	// P6-GIT-05: a failure after `git worktree add` must not leak a
	// DB-invisible checkout + branch.
	cleanupOrphan := func() {
		removeOrphanWorktree(ctx, stdout, r, localPath, wtPath, branch)
	}
	if err := applyWorktreeLFSPolicy(ctx, stdout, r, project, wtPath); err != nil {
		cleanupOrphan()
		return state.Worktree{}, err
	}
	device, err := store.CurrentDevice(ctx)
	if err != nil {
		cleanupOrphan()
		return state.Worktree{}, err
	}
	wt, err := store.InsertWorktree(ctx, state.Worktree{
		NamespaceID: project.ID,
		DeviceID:    device.ID,
		Path:        wtPath,
		Branch:      branch,
		BaseRef:     baseRef,
		BaseSHA:     baseSHA,
		CreatedBy:   createdBy,
		DirtyState:  "clean",
	})
	if err != nil {
		cleanupOrphan()
		return state.Worktree{}, err
	}
	return wt, nil
}

// resolveWorktreeDefaultBranch determines the base branch for a fresh worktree.
// It prefers the authoritative remote answer (git ls-remote --symref origin HEAD)
// so a clone with no/stale refs/remotes/origin/HEAD still bases on the real
// default branch, and only falls back to the local origin/HEAD + stored
// fallback resolution when the remote query is unavailable. A non-authoritative
// resolution is surfaced to the user so a wrong base never happens silently.
func resolveWorktreeDefaultBranch(ctx context.Context, stdout io.Writer, r dsgit.Runner, localPath, fallback string) (string, error) {
	if branch, err := r.RemoteDefaultBranch(ctx, localPath, "origin"); err == nil {
		return branch, nil
	}
	branch, source, err := r.ResolveDefaultBranch(ctx, localPath, fallback)
	if err != nil {
		return "", err
	}
	if source != dsgit.DefaultBranchRemote {
		_, _ = fmt.Fprintf(stdout, "warning: could not confirm origin default branch from the remote; using %q (source: %s)\n", branch, source)
	}
	return branch, nil
}

func applyWorktreeLFSPolicy(ctx context.Context, stdout io.Writer, r dsgit.Runner, project state.ProjectStatus, wtPath string) error {
	usesLFS, err := dsgit.UsesLFS(ctx, wtPath)
	if err != nil {
		return appError{code: exitGit, err: err}
	}
	if !usesLFS {
		return nil
	}
	policy := strings.ToLower(strings.TrimSpace(project.LFSPolicy))
	if policy == "" {
		policy = "auto"
	}
	switch policy {
	case "always", "agent":
		if err := r.LFSPull(ctx, wtPath); err != nil {
			return appError{code: exitGit, err: fmt.Errorf("worktree created at %s but LFS pull failed; objects may remain pointer files: %w", wtPath, err)}
		}
	case "auto", "never":
		_, _ = fmt.Fprintf(stdout, "warning: %s uses Git LFS; worktree %s may contain pointer files (lfs_policy=%s)\n", project.Path, wtPath, policy)
	default:
		return appError{code: exitInvalidConfig, err: fmt.Errorf("unsupported lfs_policy %q for %s", project.LFSPolicy, project.Path)}
	}
	return nil
}

func validLFSPolicy(policy string) bool {
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case "auto", "never", "agent", "always":
		return true
	default:
		return false
	}
}

type worktreeStatusOutput struct {
	ID         string `json:"id"`
	Path       string `json:"path"`
	Branch     string `json:"branch"`
	BaseRef    string `json:"base_ref"`
	BaseSHA    string `json:"base_sha"`
	CurrentSHA string `json:"current_sha"`
	Fresh      bool   `json:"fresh"`
	Behind     int    `json:"behind"`
	DirtyState string `json:"dirty_state"`
}

func newWorktreeStatusCommand(stdout io.Writer, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "status <id>",
		Short: "Check worktree freshness against its recorded upstream base",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			wt, err := store.WorktreeByID(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			r := gitRunner(opts)
			drift, err := r.BaseDrift(cmd.Context(), wt.Path, wt.BaseRef, wt.BaseSHA)
			if err != nil {
				return appError{code: exitGit, err: err}
			}
			dirty, err := r.DirtyState(cmd.Context(), wt.Path)
			if err != nil {
				dirty = dsgit.DirtyUnknown
			}
			out := worktreeStatusOutput{
				ID:         wt.ID,
				Path:       wt.Path,
				Branch:     wt.Branch,
				BaseRef:    wt.BaseRef,
				BaseSHA:    wt.BaseSHA,
				CurrentSHA: drift.CurrentSHA,
				Fresh:      drift.Fresh,
				Behind:     drift.Behind,
				DirtyState: string(dirty),
			}
			return opts.render(stdout, func(w io.Writer) error {
				status := "fresh"
				if !drift.Fresh {
					status = fmt.Sprintf("stale (behind %d)", drift.Behind)
				}
				_, err = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", wt.ID, status, wt.BaseRef, drift.CurrentSHA, dirty)
				return err
			}, out)
		},
	}
}

type worktreeFinalizeResult struct {
	ID         string `json:"id"`
	BaseRef    string `json:"base_ref"`
	BaseSHA    string `json:"base_sha"`
	CurrentSHA string `json:"current_sha"`
	Fresh      bool   `json:"fresh"`
	Behind     int    `json:"behind"`
}

func newWorktreeFinalizeCommand(stdout io.Writer, opts *options) *cobra.Command {
	var allowStaleBase bool
	cmd := &cobra.Command{
		Use:   "finalize <id>",
		Short: "Run final stale-base checks before PR or handoff",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			wt, err := store.WorktreeByID(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			drift, err := finalizationBaseDrift(cmd.Context(), opts, wt)
			if err != nil {
				return err
			}
			if !drift.Fresh && !allowStaleBase {
				return appError{code: exitConflict, err: fmt.Errorf("base %s moved %d commits; rebase or pass --allow-stale-base", wt.BaseRef, drift.Behind)}
			}
			out := worktreeFinalizeResult{
				ID:         wt.ID,
				BaseRef:    wt.BaseRef,
				BaseSHA:    wt.BaseSHA,
				CurrentSHA: drift.CurrentSHA,
				Fresh:      drift.Fresh,
				Behind:     drift.Behind,
			}
			return opts.render(stdout, func(w io.Writer) error {
				if !out.Fresh {
					_, err = fmt.Fprintf(w, "Warning: finalizing stale worktree %s; %s moved %d commits to %s\n", out.ID, out.BaseRef, out.Behind, out.CurrentSHA)
					return err
				}
				_, err = fmt.Fprintf(w, "Worktree %s is ready for finalization; %s is still at %s\n", out.ID, out.BaseRef, out.BaseSHA)
				return err
			}, out)
		},
	}
	cmd.Flags().BoolVar(&allowStaleBase, "allow-stale-base", false, "allow finalization even when the recorded base moved")
	return cmd
}

func finalizationBaseDrift(ctx context.Context, opts *options, wt state.Worktree) (dsgit.BaseDrift, error) {
	r := gitRunner(opts)
	drift, err := r.BaseDrift(ctx, wt.Path, wt.BaseRef, wt.BaseSHA)
	if err != nil {
		return dsgit.BaseDrift{}, appError{code: exitGit, err: err}
	}
	return drift, nil
}

func newWorktreeListCommand(stdout io.Writer, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List active worktrees",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			worktrees, err := store.ListWorktrees(cmd.Context())
			if err != nil {
				return err
			}
			return opts.render(stdout, func(w io.Writer) error {
				for _, wt := range worktrees {
					_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", wt.ID, wt.Branch, wt.BaseRef, wt.Path)
				}
				return nil
			}, worktrees)
		},
	}
}

type worktreeRemoveResult struct {
	ID     string `json:"id"`
	Pruned bool   `json:"pruned"`
}

func newWorktreeRemoveCommand(stdout io.Writer, opts *options) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "remove <id>",
		Short: "Mark a worktree removed",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			wt, err := store.WorktreeByID(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			project, err := store.ProjectByID(cmd.Context(), wt.NamespaceID)
			if err != nil {
				return err
			}
			r := gitRunner(opts)
			repoPath := project.LocalPath
			if repoPath == "" {
				repoPath = filepath.Join(opts.paths().Root, filepath.FromSlash(project.Path))
			}
			if _, err := os.Stat(wt.Path); err != nil {
				if !os.IsNotExist(err) {
					return fmt.Errorf("stat worktree: %w", err)
				}
				if !force {
					return appError{code: exitInvalidConfig, err: fmt.Errorf("worktree path %s is missing; pass --force to prune stale Git metadata and mark it removed", wt.Path)}
				}
				if dsgit.IsRepo(repoPath) {
					if err := r.WorktreePrune(cmd.Context(), repoPath); err != nil {
						return appError{code: exitGit, err: err}
					}
				}
				if err := store.MarkWorktreeRemoved(cmd.Context(), args[0]); err != nil {
					return err
				}
				out := worktreeRemoveResult{ID: args[0], Pruned: true}
				return opts.render(stdout, func(w io.Writer) error {
					_, err := fmt.Fprintf(w, "Pruned missing worktree %s\n", out.ID)
					return err
				}, out)
			}
			dirty, err := r.DirtyState(cmd.Context(), wt.Path)
			if err != nil {
				return err
			}
			if dirty != dsgit.DirtyClean && !force {
				return appError{code: exitDirtyWorktree, err: fmt.Errorf("refusing to remove dirty worktree %s: %s", wt.Path, dirty)}
			}
			if err := r.WorktreeRemove(cmd.Context(), repoPath, wt.Path, force); err != nil {
				return appError{code: exitGit, err: err}
			}
			if err := store.MarkWorktreeRemoved(cmd.Context(), args[0]); err != nil {
				return err
			}
			out := worktreeRemoveResult{ID: args[0], Pruned: false}
			return opts.render(stdout, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "Removed worktree %s\n", out.ID)
				return err
			}, out)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "remove dirty or missing worktrees and prune stale Git metadata")
	return cmd
}

type worktreeReapEntry struct {
	ID         string `json:"id"`
	Branch     string `json:"branch"`
	MergeLabel string `json:"merge_label"`
	BranchTip  string `json:"branch_tip,omitempty"`
}

type worktreeCleanupResult struct {
	Removed int                 `json:"removed"`
	Skipped int                 `json:"skipped"`
	Reaped  []worktreeReapEntry `json:"reaped,omitempty"`
}

func newWorktreeCleanupCommand(stdout io.Writer, opts *options) *cobra.Command {
	var merged bool
	var force bool
	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Clean up eligible worktrees",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !merged {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("--merged is required")}
			}
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			// Reconcile crashed runs first so a dead recorder cannot spuriously
			// block cleanup of a worktree that is no longer live (P7-GIT-01).
			if _, _, err := sweepStaleAgentRuns(cmd.Context(), store); err != nil {
				return err
			}
			worktrees, err := store.ListWorktrees(cmd.Context())
			if err != nil {
				return err
			}
			removed := 0
			skipped := 0
			var reaped []worktreeReapEntry
			// One base fetch per (project, base ref) per run — N worktrees on
			// the same project must not trigger N redundant network fetches
			// (review finding).
			refreshed := map[string]bool{}
			stderr := cmd.ErrOrStderr()
			for _, wt := range worktrees {
				r := gitRunner(opts)
				project, err := store.ProjectByID(cmd.Context(), wt.NamespaceID)
				if err != nil {
					return err
				}
				repoPath := project.LocalPath
				if repoPath == "" {
					repoPath = filepath.Join(opts.paths().Root, filepath.FromSlash(project.Path))
				}
				entry, err := cleanupOneWorktree(cmd.Context(), opts, stderr, store, r, project, repoPath, wt, force, refreshed)
				if err != nil {
					return err
				}
				if entry != nil {
					removed++
					reaped = append(reaped, *entry)
				} else {
					skipped++
				}
			}
			out := worktreeCleanupResult{Removed: removed, Skipped: skipped, Reaped: reaped}
			return opts.render(stdout, func(w io.Writer) error {
				for _, e := range out.Reaped {
					// Path-missing reaps historically had no per-worktree line
					// (only the final summary); skip empty merge labels.
					if e.MergeLabel == "" {
						continue
					}
					if e.BranchTip != "" {
						if _, err := fmt.Fprintf(w, "Removed worktree %s (%s; branch %s was at %s)\n", e.ID, e.MergeLabel, e.Branch, e.BranchTip); err != nil {
							return err
						}
					} else if _, err := fmt.Fprintf(w, "Removed worktree %s (%s)\n", e.ID, e.MergeLabel); err != nil {
						return err
					}
				}
				_, err := fmt.Fprintf(w, "Cleaned up %d worktrees (%d skipped)\n", out.Removed, out.Skipped)
				return err
			}, out)
		},
	}
	cmd.Flags().BoolVar(&merged, "merged", false, "only remove merged, clean worktrees")
	cmd.Flags().BoolVar(&force, "force", false, "also remove merged worktrees with a dirty tree")
	return cmd
}

// cleanupOneWorktree reaps one worktree — missing-path metadata prune or full
// remove — entirely under the project repo lock (P7-GIT-01/02). entry==nil
// with err==nil means skip; a non-nil entry means reaped. Diagnostic warnings
// go to stderr so --json stdout stays a pure document.
func cleanupOneWorktree(ctx context.Context, opts *options, stderr io.Writer, store *state.Store, r dsgit.Runner, project state.ProjectStatus, repoPath string, wt state.Worktree, force bool, refreshed map[string]bool) (entry *worktreeReapEntry, err error) {
	unlock, err := acquireRepoLock(opts.paths().Home, project.ID)
	if err != nil {
		var app appError
		if errors.As(err, &app) && app.code == exitConflict {
			logging.Logger(ctx).Warn("worktree cleanup skipped: repo lock held", "worktree", wt.ID, "project", project.ID, "error", err.Error())
			return nil, nil
		}
		return nil, err
	}
	defer unlock()

	// The running-run check happens UNDER the lock: `agent run` holds the same
	// lock from worktree creation through InsertAgentRun, so a fresh agent
	// worktree can never be observed here without its running row.
	runs, err := store.RunningAgentRunsByWorktree(ctx, wt.ID)
	if err != nil {
		return nil, err
	}
	if len(runs) > 0 {
		logging.Logger(ctx).Warn("worktree cleanup skipped: running agent run", "run", runs[0].ID, "worktree", wt.ID)
		return nil, nil
	}

	if _, err := os.Stat(wt.Path); err != nil {
		if os.IsNotExist(err) {
			// Path-missing: metadata-only prune, under the same repo lock so
			// `git worktree prune` cannot race a concurrent `worktree new`
			// mutating .git/worktrees (P7-GIT-02 review follow-up).
			if dsgit.IsRepo(repoPath) {
				_ = r.WorktreePrune(ctx, repoPath)
			}
			if err := store.MarkWorktreeRemoved(ctx, wt.ID); err != nil {
				return nil, err
			}
			// No merge label / tip: historically only the summary counted this
			// reap (no "Removed worktree …" line).
			return &worktreeReapEntry{ID: wt.ID, Branch: wt.Branch}, nil
		}
		// Unreadable path is an error, not "missing": surface it instead of
		// silently leaving the worktree behind forever.
		logging.Logger(ctx).Warn("worktree cleanup skipped: stat failed", "worktree", wt.ID, "path", wt.Path, "error", err.Error())
		return nil, nil
	}

	dirty, err := r.DirtyState(ctx, wt.Path)
	if err != nil {
		logging.Logger(ctx).Warn("worktree cleanup skipped: dirty-state check failed", "worktree", wt.ID, "path", wt.Path, "error", err.Error())
		return nil, nil
	}
	if dirty != dsgit.DirtyClean && !force {
		return nil, nil
	}
	if refreshKey := project.ID + "\x00" + wt.BaseRef; !refreshed[refreshKey] {
		refreshed[refreshKey] = true
		if err := refreshWorktreeBaseLocked(ctx, r, repoPath, wt.BaseRef); err != nil {
			_, _ = fmt.Fprintf(stderr, "warning: could not refresh %s for worktree %s: %v; using local ref\n", wt.BaseRef, wt.ID, err)
		}
	}
	mergeLabel := "merged"
	mergedOut, err := r.Run(ctx, wt.Path, "branch", "--merged", wt.BaseRef, "--list", wt.Branch)
	if err != nil || !strings.Contains(mergedOut, wt.Branch) {
		squashMerged, squashErr := r.IsSquashMerged(ctx, wt.Path, wt.Branch, wt.BaseRef)
		if squashErr != nil || !squashMerged {
			return nil, nil
		}
		mergeLabel = "merged (squash)"
	}
	// P7-GIT-01 TOCTOU re-check: DirtyState again immediately before remove,
	// under the held repo lock, so concurrent edits after the first check
	// cannot be reaped without --force.
	dirty, err = r.DirtyState(ctx, wt.Path)
	if err != nil {
		logging.Logger(ctx).Warn("worktree cleanup skipped: dirty-state check failed", "worktree", wt.ID, "path", wt.Path, "error", err.Error())
		return nil, nil
	}
	if dirty != dsgit.DirtyClean && !force {
		return nil, nil
	}
	// Recovery breadcrumb: content-equivalence can match a
	// coincidentally-identical unrelated commit (documented
	// limitation), so name the deleted branch's tip — recreating
	// it is one `git branch <name> <sha>` away until git gc.
	tip := ""
	if out, terr := r.RevParse(ctx, repoPath, wt.Branch); terr == nil {
		tip = strings.TrimSpace(out)
	}
	if err := r.WorktreeRemove(ctx, repoPath, wt.Path, force); err != nil {
		logging.Logger(ctx).Warn("worktree cleanup skipped: removal failed", "worktree", wt.ID, "path", wt.Path, "error", err.Error())
		return nil, nil
	}
	if _, err := r.Run(ctx, repoPath, "branch", "-D", wt.Branch); err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: failed to delete branch %s for removed worktree %s: %v\n", wt.Branch, wt.ID, err)
	}
	if err := store.MarkWorktreeRemoved(ctx, wt.ID); err != nil {
		return nil, err
	}
	return &worktreeReapEntry{
		ID:         wt.ID,
		Branch:     wt.Branch,
		MergeLabel: mergeLabel,
		BranchTip:  tip,
	}, nil
}

// refreshWorktreeBaseLocked parses remote/branch and fetches. Caller must hold
// the project repo lock (its only caller, cleanupOneWorktree, acquires it for
// the whole reap sequence — a lock-taking wrapper here would deadlock).
func refreshWorktreeBaseLocked(ctx context.Context, r dsgit.Runner, repoPath, baseRef string) error {
	remote, branch, ok := strings.Cut(baseRef, "/")
	if !ok || remote == "" || branch == "" {
		return fmt.Errorf("base ref must be remote/branch, got %q", baseRef)
	}
	return r.Fetch(ctx, repoPath, remote, branch)
}

var slugPattern = regexp.MustCompile(`[^a-z0-9]+`)

const worktreeBranchAttempts = 3

var (
	worktreeNow        = func() time.Time { return time.Now().UTC() }
	worktreeSuffixFunc = worktreeSuffix
)

type worktreeAdder interface {
	WorktreeAdd(ctx context.Context, dir, path, branch, base string) error
}

func addWorktreeWithFreshBranch(ctx context.Context, runner worktreeAdder, home, projectID, localPath, slug, baseSHA string) (string, string, error) {
	var lastErr error
	for attempt := 0; attempt < worktreeBranchAttempts; attempt++ {
		branch, err := newWorktreeBranch(slug)
		if err != nil {
			return "", "", err
		}
		wtPath := filepath.Join(home, "worktrees", projectID, strings.ReplaceAll(branch, "/", "-"))
		//nolint:gosec // Worktree parent directories live under DevStrap home and contain checkouts, not private key material.
		if err := os.MkdirAll(filepath.Dir(wtPath), 0o755); err != nil {
			return "", "", fmt.Errorf("create worktree parent: %w", err)
		}
		if err := runner.WorktreeAdd(ctx, localPath, wtPath, branch, baseSHA); err != nil {
			lastErr = err
			if isGitBranchExistsError(err) {
				continue
			}
			return "", "", err
		}
		return branch, wtPath, nil
	}
	return "", "", fmt.Errorf("create unique worktree branch after %d attempts: %w", worktreeBranchAttempts, lastErr)
}

func newWorktreeBranch(slug string) (string, error) {
	suffix, err := worktreeSuffixFunc(12)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("agent/%s-%s-%s", slug, worktreeNow().UTC().Format("20060102-150405"), suffix), nil
}

func worktreeSuffix(length int) (string, error) {
	if length <= 0 {
		return "", fmt.Errorf("suffix length must be positive")
	}
	raw := make([]byte, (length+1)/2)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate worktree branch suffix: %w", err)
	}
	return hex.EncodeToString(raw)[:length], nil
}

func isGitBranchExistsError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "branch") && strings.Contains(msg, "already exists")
}

func slugify(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = slugPattern.ReplaceAllString(value, "-")
	value = strings.Trim(value, "-")
	if value == "" {
		return "task"
	}
	if len(value) > 40 {
		value = strings.Trim(value[:40], "-")
	}
	return value
}

// removeOrphanWorktree force-removes a just-created worktree checkout and
// deletes its branch (in that order — git refuses to delete a branch that is
// still checked out in a live worktree). The cleanup context is detached from
// the caller's ctx with its own bound, because the failure being cleaned up
// may BE a cancellation (Ctrl-C mid-LFS-pull) — running cleanup under the
// same cancelled ctx would no-op and leak the exact orphan this exists to
// remove. Failures are surfaced as warnings (not swallowed) so an operator
// knows manual cleanup is needed (P6-GIT-05).
func removeOrphanWorktree(ctx context.Context, warn io.Writer, r dsgit.Runner, repoPath, wtPath, branch string) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	if err := r.WorktreeRemove(cctx, repoPath, wtPath, true); err != nil {
		_, _ = fmt.Fprintf(warn, "warning: failed to remove orphaned worktree %s: %v (remove it manually, then 'git worktree prune')\n", wtPath, err)
	}
	if _, err := r.Run(cctx, repoPath, "branch", "-D", branch); err != nil {
		_, _ = fmt.Fprintf(warn, "warning: failed to delete orphaned branch %s in %s: %v\n", branch, repoPath, err)
	}
}
