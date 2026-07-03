package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
		Args:  cobra.ExactArgs(1),
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
			if opts.v.GetBool("json") {
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(report)
			}
			switch {
			case !held:
				_, err = fmt.Fprintf(stdout, "No repo lock held for %s\n", project.Path)
			case report.Cleared:
				_, err = fmt.Fprintf(stdout, "Cleared %s repo lock for %s (pid %d on %s, acquired %s)\n", staleLabel(stale), project.Path, info.PID, info.Hostname, info.AcquiredAt)
			default:
				_, err = fmt.Fprintf(stdout, "Repo lock for %s held by pid %d on %s (acquired %s)\n", project.Path, info.PID, info.Hostname, info.AcquiredAt)
			}
			return err
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
		Args:  cobra.ExactArgs(1),
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
			_, err = fmt.Fprintf(stdout, "Created worktree %s at %s from %s %s\n", wt.Branch, wt.Path, wt.BaseRef, wt.BaseSHA)
			return err
		},
	}
	cmd.Flags().BoolVar(&freshUpstream, "fresh-upstream", false, "base the worktree on fetched remote default branch")
	cmd.Flags().StringVar(&taskName, "name", "", "task name for branch slug")
	return cmd
}

func createFreshWorktree(ctx context.Context, stdout io.Writer, opts *options, store *state.Store, project state.ProjectStatus, taskName, createdBy string) (state.Worktree, error) {
	// NOVCS-04: preflight — a remote-less repo cannot produce a fresh-upstream
	// worktree; fail fast with an actionable message before touching git.
	if strings.TrimSpace(project.RemoteKey) == "" {
		return state.Worktree{}, appError{code: exitInvalidConfig, err: fmt.Errorf("%s has no git remote; fresh-upstream worktrees require one (add one with 'git remote add origin <url>')", project.Path)}
	}
	unlock, err := acquireRepoLock(opts.paths().Home, project.ID)
	if err != nil {
		return state.Worktree{}, err
	}
	defer unlock()
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
		Args:  cobra.ExactArgs(1),
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
			if opts.v.GetBool("json") {
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}
			status := "fresh"
			if !drift.Fresh {
				status = fmt.Sprintf("stale (behind %d)", drift.Behind)
			}
			_, err = fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\t%s\n", wt.ID, status, wt.BaseRef, drift.CurrentSHA, dirty)
			return err
		},
	}
}

func newWorktreeFinalizeCommand(stdout io.Writer, opts *options) *cobra.Command {
	var allowStaleBase bool
	cmd := &cobra.Command{
		Use:   "finalize <id>",
		Short: "Run final stale-base checks before PR or handoff",
		Args:  cobra.ExactArgs(1),
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
			if !drift.Fresh {
				_, err = fmt.Fprintf(stdout, "Warning: finalizing stale worktree %s; %s moved %d commits to %s\n", wt.ID, wt.BaseRef, drift.Behind, drift.CurrentSHA)
				return err
			}
			_, err = fmt.Fprintf(stdout, "Worktree %s is ready for finalization; %s is still at %s\n", wt.ID, wt.BaseRef, wt.BaseSHA)
			return err
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
			if opts.v.GetBool("json") {
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(worktrees)
			}
			for _, wt := range worktrees {
				_, _ = fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", wt.ID, wt.Branch, wt.BaseRef, wt.Path)
			}
			return nil
		},
	}
}

func newWorktreeRemoveCommand(stdout io.Writer, opts *options) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "remove <id>",
		Short: "Mark a worktree removed",
		Args:  cobra.ExactArgs(1),
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
				_, err = fmt.Fprintf(stdout, "Pruned missing worktree %s\n", args[0])
				return err
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
			_, err = fmt.Fprintf(stdout, "Removed worktree %s\n", args[0])
			return err
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "remove dirty or missing worktrees and prune stale Git metadata")
	return cmd
}

func newWorktreeCleanupCommand(stdout io.Writer, opts *options) *cobra.Command {
	var merged bool
	var force bool
	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Clean up eligible worktrees",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !merged {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("--merged is required")}
			}
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			worktrees, err := store.ListWorktrees(cmd.Context())
			if err != nil {
				return err
			}
			removed := 0
			skipped := 0
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
				if _, err := os.Stat(wt.Path); err != nil {
					if os.IsNotExist(err) {
						if dsgit.IsRepo(repoPath) {
							_ = r.WorktreePrune(cmd.Context(), repoPath)
						}
						if err := store.MarkWorktreeRemoved(cmd.Context(), wt.ID); err != nil {
							return err
						}
						removed++
						continue
					}
					// Unreadable path is an error, not "missing": surface it
					// instead of silently leaving the worktree behind forever.
					skipped++
					logging.Logger(cmd.Context()).Warn("worktree cleanup skipped: stat failed", "worktree", wt.ID, "path", wt.Path, "error", err.Error())
					continue
				}
				dirty, err := r.DirtyState(cmd.Context(), wt.Path)
				if err != nil {
					skipped++
					logging.Logger(cmd.Context()).Warn("worktree cleanup skipped: dirty-state check failed", "worktree", wt.ID, "path", wt.Path, "error", err.Error())
					continue
				}
				if dirty != dsgit.DirtyClean && !force {
					skipped++
					continue
				}
				mergedOut, err := r.Run(cmd.Context(), wt.Path, "branch", "--merged", wt.BaseRef, "--list", wt.Branch)
				if err != nil || !strings.Contains(mergedOut, wt.Branch) {
					skipped++
					continue
				}
				if err := r.WorktreeRemove(cmd.Context(), repoPath, wt.Path, force); err != nil {
					skipped++
					continue
				}
				if err := store.MarkWorktreeRemoved(cmd.Context(), wt.ID); err != nil {
					return err
				}
				removed++
			}
			_, err = fmt.Fprintf(stdout, "Cleaned up %d worktrees (%d skipped)\n", removed, skipped)
			return err
		},
	}
	cmd.Flags().BoolVar(&merged, "merged", false, "only remove merged, clean worktrees")
	cmd.Flags().BoolVar(&force, "force", false, "also remove merged worktrees with a dirty tree")
	return cmd
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
