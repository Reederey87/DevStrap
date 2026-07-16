package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	dsgit "github.com/Reederey87/DevStrap/internal/git"
	"github.com/Reederey87/DevStrap/internal/logging"
	"github.com/Reederey87/DevStrap/internal/pathkey"
	"github.com/Reederey87/DevStrap/internal/redact"
	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/cobra"
)

// hydrateResult is the --json shape for `devstrap hydrate` (P5-CLI-01 part B).
type hydrateResult struct {
	Path string `json:"path"`
}

func newHydrateCommand(stdout io.Writer, opts *options) *cobra.Command {
	var partial bool
	var full bool
	var lfs bool
	cmd := &cobra.Command{
		Use:   "hydrate <path>",
		Short: "Clone a skeleton Git repository",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if partial && full {
				return appError{code: exitInvalidConfig, err: errors.New("use either --partial or --full")}
			}
			localPath, err := hydrateProject(cmd.Context(), opts, args[0], !full)
			if err != nil {
				return err
			}
			if lfs {
				r := gitRunner(opts)
				if err := r.LFSPull(cmd.Context(), localPath); err != nil {
					return err
				}
			}
			return opts.render(stdout, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "Hydrated %s\n", localPath)
				return err
			}, hydrateResult{Path: localPath})
		},
	}
	cmd.Flags().BoolVar(&partial, "partial", true, "use partial clone with blob filtering")
	cmd.Flags().BoolVar(&full, "full", false, "use a full clone")
	cmd.Flags().BoolVar(&lfs, "lfs", false, "pull Git LFS objects after clone")
	return cmd
}

// submodulePolicy resolves the per-clone submodule materialization policy
// (GIT-06) from config: always|auto|never. "auto" and "always" both pass
// --recurse-submodules (a no-op when the repo has no submodules); "never"
// skips submodule initialization so a blobless clone stays minimal.
func submodulePolicy(opts *options) string {
	p := strings.ToLower(strings.TrimSpace(opts.v.GetString("materialization.submodules")))
	switch p {
	case "never":
		return "never"
	default:
		if p == "" {
			return "auto"
		}
		return p
	}
}

// maintenanceEnabled reports whether an opt-in one-time `git maintenance run
// --auto` should run after clone (GIT-06) so blobless clones do not trigger
// per-object lazy-fetch storms on the first blame/log -p.
func maintenanceEnabled(opts *options) bool {
	return opts.v.GetBool("materialization.maintenance")
}

const defaultCloneTimeout = 30 * time.Minute

// cloneTimeout resolves materialization.clone_timeout (P6-GIT-01): the
// per-attempt deadline for the network-transfer command class. An explicit 0
// means the transfer class runs unbounded (Runner.LongTimeout <= 0), not a
// fallback to the short 2m cap. The raw value is parsed here rather than via
// viper's GetDuration, because GetDuration maps a malformed value to 0 — which
// would silently turn a typo into "no timeout at all".
func cloneTimeout(opts *options) time.Duration {
	if opts == nil || opts.v == nil {
		return defaultCloneTimeout
	}
	raw := strings.TrimSpace(opts.v.GetString("materialization.clone_timeout"))
	if raw == "" {
		return defaultCloneTimeout
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		fmt.Fprintf(os.Stderr, "warning: invalid materialization.clone_timeout %q; using default %s\n", raw, defaultCloneTimeout)
		return defaultCloneTimeout
	}
	return d
}

func gitRunner(opts *options) dsgit.Runner {
	r := dsgit.NewRunner()
	r.LongTimeout = cloneTimeout(opts)
	return r
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
		r := gitRunner(opts)
		dirty, _ := r.DirtyState(ctx, localPath)
		_ = store.UpdateProjectLocalState(ctx, project.ID, localPath, "available", string(dirty), "")
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
	r := gitRunner(opts)
	// GIT-06: initialize submodules unless the policy is "never" so the
	// working tree is structurally complete; with a blobless clone, keep the
	// submodules blobless too (--also-filter-submodules).
	submodules := submodulePolicy(opts) != "never"
	if err := r.CloneWithOptions(ctx, project.RemoteURL, tmpPath, dsgit.CloneOptions{
		Partial:              partial,
		Submodules:           submodules,
		AlsoFilterSubmodules: partial && submodules,
	}); err != nil {
		_ = store.UpdateProjectLocalState(ctx, project.ID, localPath, "failed", "unknown", redact.Scrub(fmt.Sprintf("clone: %v", err)))
		return "", err
	}
	if err := promoteClonedRepo(tmpPath, localPath, project.Path, project.RemoteURL); err != nil {
		_ = store.UpdateProjectLocalState(ctx, project.ID, localPath, "failed", "unknown", redact.Scrub(fmt.Sprintf("promote clone: %v", err)))
		return "", err
	}
	cleanupTmp = false
	// GIT-01: verify the clone produced a usable checkout before recording it
	// available/clean. A broken/stale remote whose advertised HEAD points at a
	// ref absent from the fetched refs leaves an empty/detached checkout
	// ("remote HEAD refers to nonexistent ref, unable to checkout"); DirtyState
	// on that empty tree returns clean, so without this guard the project would
	// be silently recorded as available/clean and the "tree is really present
	// on disk" promise would break invisibly. A legitimately empty repo (no
	// commits yet, unborn branch) is distinct: it is a valid, present repo and
	// is recorded as available. Attempt one self-heal (re-resolve the remote
	// default branch and check it out); only a repo that has commits but no
	// resolvable HEAD after self-heal is recorded as materialized-empty.
	if !headResolvable(ctx, r, localPath) {
		selfHealCheckout(ctx, r, localPath)
	}
	matState, dirty := "available", "unknown"
	switch {
	case headResolvable(ctx, r, localPath):
		if d, derr := r.DirtyState(ctx, localPath); derr == nil {
			dirty = string(d)
		}
	case repoHasCommits(ctx, r, localPath):
		// Commits exist but HEAD cannot resolve even after self-heal: the
		// remote HEAD is broken. Record an honest state, not available/clean.
		matState = "materialized-empty"
	default:
		// No commits at all: a legitimately empty repo (fresh remote, nothing
		// pushed yet). It is present and clean; record it as available so
		// hydrating a brand-new remote succeeds.
		dirty = "clean"
	}
	lastError := ""
	if matState == "materialized-empty" {
		lastError = redact.Scrub("clone produced an empty/broken checkout (remote HEAD may be broken)")
	}
	if err := store.UpdateProjectLocalState(ctx, project.ID, localPath, matState, dirty, lastError); err != nil {
		return "", err
	}
	if matState == "materialized-empty" {
		return localPath, appError{code: exitGit, err: fmt.Errorf("%s cloned but checkout is empty (remote HEAD may be broken); recorded as materialized-empty", project.Path)}
	}
	// GIT-06: opt-in one-time maintenance so a blobless clone does not trigger
	// per-object lazy-fetch storms on the first blame/log -p. Best-effort:
	// older git or a missing promisor makes this a no-op/error; never fail
	// materialization on it.
	if maintenanceEnabled(opts) {
		_ = r.MaintenanceRun(ctx, localPath)
	}
	return localPath, nil
}

// applyMaterializeLFSPolicy mirrors applyWorktreeLFSPolicy for the primary
// materialize/hydrate clone path (P6-GIT-04). Unlike the worktree path it runs
// `git lfs install --local` first because gitEnv hides the user's global smudge
// filter. LFSPull already carries the P6-GIT-01 long-transfer timeout.
func applyMaterializeLFSPolicy(ctx context.Context, r dsgit.Runner, project state.ProjectStatus, localPath string) error {
	usesLFS, err := dsgit.UsesLFS(ctx, localPath)
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
		if err := r.LFSInstallLocal(ctx, localPath); err != nil {
			return appError{code: exitGit, err: fmt.Errorf("%s: git lfs install --local failed: %w", project.Path, err)}
		}
		if err := r.LFSPull(ctx, localPath); err != nil {
			return appError{code: exitGit, err: fmt.Errorf("%s materialized but LFS pull failed; objects may remain pointer files: %w", project.Path, err)}
		}
	case "auto", "never":
		logging.Logger(ctx).Warn("git LFS pointer files remain; set lfs_policy=always to fetch objects", "path", project.Path, "lfs_policy", policy)
	default:
		return appError{code: exitInvalidConfig, err: fmt.Errorf("unsupported lfs_policy %q for %s", project.LFSPolicy, project.Path)}
	}
	return nil
}

// headResolvable reports whether the repo at localPath has a resolvable HEAD
// (GIT-01). Both a legitimately empty repo (unborn branch) and a broken-HEAD
// clone fail this check; repoHasCommits distinguishes them.
func headResolvable(ctx context.Context, r dsgit.Runner, localPath string) bool {
	_, err := r.Run(ctx, localPath, "rev-parse", "--verify", "HEAD")
	return err == nil
}

// repoHasCommits reports whether the repo at localPath has any commits
// reachable from any ref (GIT-01). This distinguishes a legitimately empty
// repo (no commits, unborn branch) from a broken-HEAD clone that has commits
// on branches HEAD does not point at.
func repoHasCommits(ctx context.Context, r dsgit.Runner, localPath string) bool {
	out, err := r.Run(ctx, localPath, "rev-list", "--count", "--all")
	if err != nil {
		return false
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	return err == nil && n > 0
}

// selfHealCheckout attempts to repair an empty checkout by re-resolving the
// remote default branch and checking it out (GIT-01). Failures are best-effort
// and swallowed; the caller records an honest state regardless of outcome.
func selfHealCheckout(ctx context.Context, r dsgit.Runner, localPath string) {
	branch, _, err := r.ResolveDefaultBranch(ctx, localPath, "")
	if err != nil || branch == "" {
		return
	}
	_, _ = r.Run(ctx, localPath, "checkout", branch)
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
