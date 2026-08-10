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
	"github.com/Reederey87/DevStrap/internal/redact"
	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/cobra"
)

func newWorktreeCommand(stdout io.Writer, opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "worktree",
		Short: "Manage isolated worktrees",
	}
	cmd.AddCommand(newWorktreeNewCommand(stdout, opts))
	cmd.AddCommand(newWorktreeAdoptCommand(stdout, opts))
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
			wt, err := createFreshWorktree(cmd.Context(), stdout, cmd.ErrOrStderr(), opts, store, project, taskName, "agent")
			if err != nil {
				return err
			}
			result := newWorktreeProvisionResult(opts.paths().Root, project, wt)
			return opts.render(stdout, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "Created worktree %s at %s from %s %s\n", wt.Branch, wt.Path, wt.BaseRef, wt.BaseSHA)
				return err
			}, result)
		},
	}
	cmd.Flags().BoolVar(&freshUpstream, "fresh-upstream", false, "base the worktree on fetched remote default branch")
	cmd.Flags().StringVar(&taskName, "name", "", "task name for branch slug")
	return cmd
}

// worktreeProvisionResult is the machine contract for `worktree new --json`
// (AD5-01). It EMBEDS state.Worktree so every field that shipped before keeps
// its exact name and position — this is an additive extension, not a reshape,
// so existing --json consumers are unaffected (spec/13's P5-CLI-01
// migration/compat rule). The added fields are the ones an external agent
// harness cannot proceed without: it receives an opaque namespace_id today and
// has no way to learn the project path, the remote, the branch that was
// resolved, or where the main checkout lives.
type worktreeProvisionResult struct {
	state.Worktree
	SchemaVersion int      `json:"schema_version"`
	ProjectPath   string   `json:"project_path"`
	RemoteURL     string   `json:"remote_url,omitempty"`
	DefaultBranch string   `json:"default_branch,omitempty"`
	RepoPath      string   `json:"repo_path,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
}

// worktreeProvisionSchemaVersion is the contract version for
// worktreeProvisionResult. Bump ONLY for an additive change; a field rename or
// removal is a breaking change that needs a deliberate decision, not a bump.
const worktreeProvisionSchemaVersion = 1

// newWorktreeProvisionResult assembles the machine contract from the pieces the
// command already has. It exists as a separate function so the contract tests
// exercise the REAL assembly — in particular the redaction of remote_url. A test
// that called redact.StripURLUserinfo itself and then asserted the output is
// clean would pass even if this function stopped redacting, which is the
// can-never-fail test class this project has caught in review twice.
func newWorktreeProvisionResult(root string, project state.ProjectStatus, wt state.Worktree) worktreeProvisionResult {
	repoPath := project.LocalPath
	if repoPath == "" {
		repoPath = filepath.Join(root, filepath.FromSlash(project.Path))
	}
	// BaseRef is always "origin/<default_branch>"; if it does not cut, leave the
	// field empty rather than guessing at a branch name.
	var defaultBranch string
	if _, branch, ok := strings.Cut(wt.BaseRef, "/"); ok {
		defaultBranch = branch
	}
	return worktreeProvisionResult{
		Worktree:      wt,
		SchemaVersion: worktreeProvisionSchemaVersion,
		ProjectPath:   project.Path,
		// A remote URL can carry credentials, and this payload is designed to be
		// read by third-party programs. StripURLUserinfo (not redact.URL) keeps
		// the URL USABLE — it drops the whole userinfo for http/https and keeps
		// only the SSH login name for ssh/git — which is what a harness needs.
		//
		// Note the scp-like form (git@github.com:org/repo.git) is passed through
		// UNCHANGED: url.Parse rejects it ("first path segment in URL cannot
		// contain colon"), so StripURLUserinfo returns it verbatim. That is safe,
		// not an oversight — git's scp-like syntax is [user@]host:path with no
		// password-embedding mechanism, so the only thing that can ride through
		// is the SSH login name, which the ssh:// branch preserves deliberately
		// too. TestWorktreeProvisionResultRemoteURLShapes pins every shape.
		RemoteURL:     redact.StripURLUserinfo(project.RemoteURL),
		DefaultBranch: defaultBranch,
		RepoPath:      repoPath,
	}
}

func createFreshWorktree(ctx context.Context, stdout, stderr io.Writer, opts *options, store *state.Store, project state.ProjectStatus, taskName, createdBy string) (state.Worktree, error) {
	unlock, err := acquireRepoLock(opts.paths().Home, project.ID)
	if err != nil {
		return state.Worktree{}, err
	}
	defer unlock()
	return createFreshWorktreeLocked(ctx, stdout, stderr, opts, store, project, taskName, createdBy)
}

// createFreshWorktreeLocked is createFreshWorktree for callers that already
// hold the project repo lock — `agent run` keeps it held until the running
// agent_runs row exists, so `worktree cleanup` can never observe the fresh
// worktree without its run row (P7-GIT-01 startup window).
//
// stdout is passed through to removeOrphanWorktree's failure-path cleanup
// only (no --json success document follows an error return, so writes there
// cannot corrupt one); the two non-fatal advisory warnings below
// (resolveWorktreeDefaultBranch, applyWorktreeLFSPolicy) fire on the SUCCESS
// path that DOES emit a --json document afterward, so they route to stderr
// instead (P5-CLI-01 part B purity fix).
func createFreshWorktreeLocked(ctx context.Context, stdout, stderr io.Writer, opts *options, store *state.Store, project state.ProjectStatus, taskName, createdBy string) (state.Worktree, error) {
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
	defaultBranch, err := resolveWorktreeDefaultBranch(ctx, stderr, r, localPath, project.DefaultBranch)
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
	if err := applyWorktreeLFSPolicy(ctx, stderr, r, project, wtPath); err != nil {
		cleanupOrphan()
		return state.Worktree{}, err
	}
	// W12-02: a fresh agent/human worktree inherits the SAME project sparse
	// profile as the primary checkout — the step most likely to be silently
	// missed, since it would otherwise defeat the whole feature for agent
	// worktrees on a monorepo. Best-effort (never fails/orphans the worktree):
	// narrowing the tree is a disk-cost optimization, and a git error here
	// should not destroy an otherwise-good fresh worktree over it.
	applyProjectSparseProfile(ctx, store, r, project, wtPath)
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
func resolveWorktreeDefaultBranch(ctx context.Context, warn io.Writer, r dsgit.Runner, localPath, fallback string) (string, error) {
	if branch, err := r.RemoteDefaultBranch(ctx, localPath, "origin"); err == nil {
		return branch, nil
	}
	branch, source, err := r.ResolveDefaultBranch(ctx, localPath, fallback)
	if err != nil {
		return "", err
	}
	if source != dsgit.DefaultBranchRemote {
		_, _ = fmt.Fprintf(warn, "warning: could not confirm origin default branch from the remote; using %q (source: %s)\n", branch, source)
	}
	return branch, nil
}

// worktreeAdoptResult is the --json payload for `worktree adopt`: it combines
// a state.Worktree with fields not on that store type (project_path, the
// idempotency markers, and any adoption warnings), so it is a named struct at
// file scope per spec/13's rule (the worktreeStatusOutput pattern). It
// deliberately has no schema_version field, and the reason is scope, not a
// claim about what siblings do: `worktree adopt` is not one of the
// machine-contract surfaces spec/13 § Machine contract surfaces enumerates,
// so adding one here would extend that list unreviewed rather than tidy up an
// inconsistency. Note the field is NOT a proxy for membership in either
// direction — several enumerated surfaces (`agent list`, `agent show`,
// `status`, and `worktree list`, whose bare array has no object to carry one)
// still have none, while agentAdoptResult carries one without being
// enumerated. Only the enumeration decides.
type worktreeAdoptResult struct {
	state.Worktree
	ProjectPath       string   `json:"project_path"`
	AlreadyAdopted    bool     `json:"already_adopted,omitempty"`
	AlreadyRegistered bool     `json:"already_registered,omitempty"`
	Warnings          []string `json:"warnings,omitempty"`
}

// adoptOutcome carries the idempotency markers and any non-fatal adoption
// warnings that both `worktree adopt` and `agent adopt --adopt-worktree` need
// in order to render their own (different) human/--json output.
type adoptOutcome struct {
	AlreadyAdopted    bool
	AlreadyRegistered bool
	Warnings          []string
}

// adoptWorktreeAt resolves an externally-created linked worktree at path and
// registers it against the project inferred from projectFlag (or the
// worktree's main checkout), refreshing an already-adopted row in place, or
// leaving a row this device registered some other way untouched. This is the
// single resolve-and-register flow behind BOTH `devstrap worktree adopt` and
// `devstrap agent adopt --adopt-worktree` (AD5-03) — two copies of this logic
// is a defect, not a shortcut, since the refusal matrix and the
// read-only-on-foreign-rows rule must not be able to drift between them.
func adoptWorktreeAt(ctx context.Context, stderr io.Writer, opts *options, store *state.Store, path, projectFlag, baseRefFlag string, allowShallow bool) (state.Worktree, state.ProjectStatus, adoptOutcome, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return state.Worktree{}, state.ProjectStatus{}, adoptOutcome{}, appError{code: exitUsage, err: fmt.Errorf("resolve path %q: %w", path, err)}
	}
	// Resolved identically to `worktree new`'s stored path (P6 path
	// normalization parity fix below) so the same physical worktree
	// hits the same idx_worktrees_active_path row regardless of which
	// command registered it, or which /var vs /private/var spelling
	// the caller used.
	resolvedPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return state.Worktree{}, state.ProjectStatus{}, adoptOutcome{}, appError{code: exitUsage, err: fmt.Errorf("%s does not exist or is not accessible: %w", path, err)}
	}
	r := gitRunner(opts)

	identity, err := r.WorktreeIdentity(ctx, resolvedPath)
	if err != nil {
		return state.Worktree{}, state.ProjectStatus{}, adoptOutcome{}, appError{code: exitUsage, err: fmt.Errorf("%s is not a git worktree: %w", resolvedPath, err)}
	}
	// WorktreeIdentity leaves MainCheckout empty for a layout whose common dir
	// is not "<checkout>/.git" (a bare repo, or a `--separate-git-dir` clone).
	// Adoption needs the main checkout to map the worktree onto an adopted
	// project, so refuse here rather than guessing — the sandbox path
	// deliberately tolerates the same case, since it only needs the common dir.
	if identity.MainCheckout == "" {
		return state.Worktree{}, state.ProjectStatus{}, adoptOutcome{}, appError{code: exitUsage, err: fmt.Errorf("cannot determine the main checkout for %s (its git-common-dir %q is not a <checkout>/.git layout — a bare repo or a --separate-git-dir clone); adopt is not supported for this layout", resolvedPath, identity.CommonDir)}
	}
	if !identity.IsLinked {
		if identity.MainCheckout == resolvedPath {
			return state.Worktree{}, state.ProjectStatus{}, adoptOutcome{}, appError{code: exitUsage, err: fmt.Errorf("%s is the main checkout, not a linked worktree; there is nothing to adopt", resolvedPath)}
		}
		return state.Worktree{}, state.ProjectStatus{}, adoptOutcome{}, appError{code: exitUsage, err: fmt.Errorf("%s is not a linked git worktree", resolvedPath)}
	}
	// Unborn HEAD (a brand new `git init`, no commits yet): there is no
	// commit to merge-base against. Detached HEAD is NOT refused here —
	// it is the common, expected shape for agent-harness worktrees and
	// is adopted with Branch == "".
	if identity.HeadSHA == "" {
		return state.Worktree{}, state.ProjectStatus{}, adoptOutcome{}, appError{code: exitUsage, err: fmt.Errorf("%s has an unborn HEAD (no commits yet); nothing to record a base against", resolvedPath)}
	}

	project, err := projectForWorktreeAdopt(ctx, opts, store, identity.MainCheckout, projectFlag)
	if err != nil {
		return state.Worktree{}, state.ProjectStatus{}, adoptOutcome{}, err
	}

	var warnings []string
	// W12-02: `adopt` registers a worktree an external harness created on its
	// own — it never mutates that checkout — so a configured sparse profile is
	// NOT applied here (unlike `worktree new`, which controls the checkout it
	// creates). Surface that as an explained warning rather than a silent
	// inconsistency: whether the adopted checkout ends up full or already
	// narrowed depends on git's own worktree-config inheritance (empirically,
	// a new linked worktree can inherit the primary checkout's active cone
	// from shared repo config even though DevStrap issued no sparse-checkout
	// command of its own), so this warning fires only when the project has a
	// configured sparse profile (gated on `len(sparsePaths) > 0` below) — it
	// does not claim to know which way the adopted checkout actually landed.
	if sparsePaths, sperr := store.SparsePathsForProject(ctx, project.ID); sperr == nil && len(sparsePaths) > 0 {
		warnings = append(warnings, fmt.Sprintf("%s has a configured sparse-checkout profile; devstrap issued no sparse-checkout command for this adopted worktree (adopt never mutates a checkout it did not create) — whether the checkout ended up narrowed depends on git's own worktree-config inheritance; to narrow it manually run 'git sparse-checkout init --cone && git sparse-checkout set -- %s' inside %s", project.Path, strings.Join(sparsePaths, " "), resolvedPath))
	}
	shallow, err := r.IsShallow(ctx, resolvedPath)
	if err != nil {
		return state.Worktree{}, state.ProjectStatus{}, adoptOutcome{}, appError{code: exitGit, err: err}
	}
	if shallow {
		if !allowShallow {
			return state.Worktree{}, state.ProjectStatus{}, adoptOutcome{}, appError{code: exitUsage, err: fmt.Errorf("%s is a shallow clone; a grafted history can make the recorded merge-base wrong at the shallow boundary — pass --allow-shallow to adopt anyway", resolvedPath)}
		}
		warnings = append(warnings, "repository is a shallow clone; recorded base_sha may be inaccurate at the shallow boundary")
	}

	baseRef := baseRefFlag
	// P8-ADOPT-03: validate the SHAPE at record time. MergeBase accepts any
	// committish, so an unvalidated --base-ref was stored verbatim and only
	// rejected much later — by BaseDrift's "remote/branch" split, with no remedy
	// named — leaving the worktree adopted but permanently unusable by
	// `worktree status`, `finalize`, and `agent pr`. `--base-ref main` is the
	// natural mistake and hit exactly that.
	//
	// refs/devstrap/* is refused outright: that namespace is the human
	// working-state plane (gitstate/WIP), and `spec/10`'s independence rule
	// keeps it strictly separate from anything an agent's base resolves from.
	// Nothing AUTOMATIC reads it — the invariant is intact — but an explicit
	// flag should not be the one door into that separation either.
	if baseRef != "" {
		if strings.HasPrefix(baseRef, "refs/devstrap/") {
			return state.Worktree{}, state.ProjectStatus{}, adoptOutcome{}, appError{code: exitUsage, err: fmt.Errorf("--base-ref %s names the DevStrap working-state plane (refs/devstrap/*), which is the human device-mirroring plane and is deliberately never a base for agent work; pass a real remote branch such as origin/main", baseRef)}
		}
		remote, branch, ok := strings.Cut(baseRef, "/")
		if !ok || remote == "" || branch == "" {
			return state.Worktree{}, state.ProjectStatus{}, adoptOutcome{}, appError{code: exitUsage, err: fmt.Errorf("--base-ref must be <remote>/<branch> (e.g. origin/main or origin/gh-pages), got %q; a bare branch name records but then fails every later freshness check", baseRef)}
		}
	}
	if baseRef == "" {
		defaultBranch, err := resolveWorktreeDefaultBranch(ctx, stderr, r, resolvedPath, project.DefaultBranch)
		if err != nil {
			return state.Worktree{}, state.ProjectStatus{}, adoptOutcome{}, appError{code: exitGit, err: err}
		}
		baseRef = "origin/" + defaultBranch
	}

	baseSHA, err := r.MergeBase(ctx, resolvedPath, identity.HeadSHA, baseRef)
	if err != nil {
		if errors.Is(err, dsgit.ErrNoMergeBase) {
			label := identity.Branch
			if label == "" {
				label = "HEAD"
			}
			return state.Worktree{}, state.ProjectStatus{}, adoptOutcome{}, appError{code: exitUsage, err: fmt.Errorf("%s and %s share no common history (adopting an orphan branch such as gh-pages is a common, legitimate case); pass an explicit --base-ref origin/<branch that shares history with %s>", label, baseRef, resolvedPath)}
		}
		return state.Worktree{}, state.ProjectStatus{}, adoptOutcome{}, appError{code: exitGit, err: err}
	}
	dirty, err := r.DirtyState(ctx, resolvedPath)
	if err != nil {
		dirty = dsgit.DirtyUnknown
	}

	// The read-then-write below runs under the project repo lock, the same
	// P7-GIT-01/02 discipline every other worktree mutation follows.
	// idx_worktrees_active_path already makes a duplicate row impossible, but
	// without the lock a concurrent adopt/new on the same path surfaces as a raw
	// SQLite constraint error instead of a clear refusal.
	unlockAdopt, err := acquireRepoLock(opts.paths().Home, project.ID)
	if err != nil {
		return state.Worktree{}, state.ProjectStatus{}, adoptOutcome{}, err
	}
	defer unlockAdopt()

	existing, lookupErr := store.WorktreeByPath(ctx, project.ID, resolvedPath)
	// Absence is the ONLY signal that licenses an insert. Any other lookup
	// failure (I/O error, corruption, timeout) must surface: reinterpreting it
	// as "not registered yet" would silently insert a second row for a worktree
	// that may already be registered.
	if lookupErr != nil && !errors.Is(lookupErr, state.ErrWorktreeNotFound) {
		return state.Worktree{}, state.ProjectStatus{}, adoptOutcome{}, lookupErr
	}
	// P8-ADOPT-07: rows written before migration 00032 stored the path as the
	// caller spelled it, because EvalSymlinks arrived with AD5-02 — i.e. with
	// adopt itself. On a symlinked prefix (/tmp -> /private/tmp is the everyday
	// case) the resolved lookup above misses such a row, and the string-keyed
	// index then admits a SECOND active row for one physical worktree.
	//
	// Retry with the unresolved spelling before concluding "not registered".
	//
	// Coverage is partial by construction: this finds the row only when the
	// caller's spelling matches the one `worktree new` happened to store. The
	// complete form would be an os.SameFile sweep over the project's active
	// rows, which catches every aliasing rather than the string-equal ones;
	// that is deliberately out of scope for a P3 that also self-heals via
	// cleanup's path-missing prune.
	if errors.Is(lookupErr, state.ErrWorktreeNotFound) && absPath != resolvedPath {
		legacy, legacyErr := store.WorktreeByPath(ctx, project.ID, absPath)
		if legacyErr != nil && !errors.Is(legacyErr, state.ErrWorktreeNotFound) {
			return state.Worktree{}, state.ProjectStatus{}, adoptOutcome{}, legacyErr
		}
		if legacyErr == nil {
			// Canonicalize the path ONLY. This row is a `worktree new` row, so
			// the branch below reports it and leaves its base untouched; the
			// path rewrite exists purely so the unique index can see it from
			// here on.
			//
			// This cannot collide: WorktreeByPath and idx_worktrees_active_path
			// share the same scope — (namespace_id, path) over status='active'
			// — so an active row already holding resolvedPath in this project
			// would have been found by the lookup above and we would never be
			// here. The UNIQUE translation is kept anyway for a racing writer
			// outside the repo lock, so SQLite text never reaches the user.
			if err := store.CanonicalizeWorktreePath(ctx, legacy.ID, resolvedPath); err != nil {
				return state.Worktree{}, state.ProjectStatus{}, adoptOutcome{}, worktreeCanonicalizePathConflict(err, absPath, resolvedPath)
			}
			legacy.Path = resolvedPath
			existing, lookupErr = legacy, nil
		}
	}
	if lookupErr == nil {
		if existing.CreatedBy != "adopted" {
			// Adoption is registration, never base-resolution: a row
			// this device created some other way (e.g. `worktree new`)
			// keeps whatever base it already recorded. Mutate nothing.
			return existing, project, adoptOutcome{AlreadyRegistered: true, Warnings: warnings}, nil
		}
		if err := store.UpdateWorktreeAdoption(ctx, existing.ID, identity.Branch, baseRef, baseSHA, string(dirty)); err != nil {
			return state.Worktree{}, state.ProjectStatus{}, adoptOutcome{}, err
		}
		existing.Branch, existing.BaseRef, existing.BaseSHA, existing.DirtyState = identity.Branch, baseRef, baseSHA, string(dirty)
		return existing, project, adoptOutcome{AlreadyAdopted: true, Warnings: warnings}, nil
	}

	device, err := store.CurrentDevice(ctx)
	if err != nil {
		return state.Worktree{}, state.ProjectStatus{}, adoptOutcome{}, err
	}
	wt, err := store.InsertWorktree(ctx, state.Worktree{
		NamespaceID: project.ID,
		DeviceID:    device.ID,
		Path:        resolvedPath,
		Branch:      identity.Branch,
		BaseRef:     baseRef,
		BaseSHA:     baseSHA,
		CreatedBy:   "adopted",
		DirtyState:  string(dirty),
	})
	if err != nil {
		// The repo lock above serializes adopt against adopt/new for this
		// project, so reaching the unique index means another process registered
		// this path outside that lock. Report it as a conflict rather than
		// leaking raw SQLite text.
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return state.Worktree{}, state.ProjectStatus{}, adoptOutcome{}, appError{code: exitConflict, err: fmt.Errorf("%s is already registered as an active worktree; run `devstrap worktree list` to see it", resolvedPath)}
		}
		return state.Worktree{}, state.ProjectStatus{}, adoptOutcome{}, err
	}
	return wt, project, adoptOutcome{Warnings: warnings}, nil
}

// worktreeCanonicalizePathConflict translates the raw SQLite UNIQUE
// constraint error from a P8-ADOPT-07 path canonicalization into a typed,
// actionable refusal naming both path spellings, instead of letting "UNIQUE
// constraint failed: ..." reach the user. An err that is not a constraint
// violation passes through unchanged.
//
// Extracted to its own function because it is not reachable through
// adoptWorktreeAt's own control flow in a single synchronous call:
// WorktreeByPath and idx_worktrees_active_path share the same scope —
// (namespace_id, path) over status='active' — so an active row already
// holding resolvedPath would have been found by the lookup that runs before
// this one, and adoptWorktreeAt would never reach the retry branch at all. A
// genuine collision can only come from a writer outside the repo lock; this
// function exists so that (rare, hard-to-race-in-a-test) case still gets a
// clear refusal instead of raw SQLite text, and so the translation itself is
// directly testable without needing to reproduce the race.
func worktreeCanonicalizePathConflict(err error, absPath, resolvedPath string) error {
	if strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return appError{code: exitConflict, err: fmt.Errorf("%s is registered twice — once as %q and once as %q — for one physical worktree; run `devstrap worktree list` and `devstrap worktree remove` the stale registration", resolvedPath, absPath, resolvedPath)}
	}
	return err
}

func newWorktreeAdoptCommand(stdout io.Writer, opts *options) *cobra.Command {
	var projectFlag string
	var baseRefFlag string
	var allowShallow bool
	cmd := &cobra.Command{
		Use:   "adopt <path>",
		Short: "Register an externally-created linked worktree (Codex/Cursor/Devin, etc.)",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			wt, project, outcome, err := adoptWorktreeAt(cmd.Context(), cmd.ErrOrStderr(), opts, store, args[0], projectFlag, baseRefFlag, allowShallow)
			if err != nil {
				return err
			}
			out := worktreeAdoptResult{Worktree: wt, ProjectPath: project.Path, AlreadyAdopted: outcome.AlreadyAdopted, AlreadyRegistered: outcome.AlreadyRegistered, Warnings: outcome.Warnings}
			return opts.render(stdout, func(w io.Writer) error {
				switch {
				case outcome.AlreadyRegistered:
					_, err := fmt.Fprintf(w, "%s is already registered as worktree %s (created_by=%s); left unchanged\n", wt.Path, wt.ID, wt.CreatedBy)
					return err
				case outcome.AlreadyAdopted:
					_, err := fmt.Fprintf(w, "Refreshed adopted worktree %s at %s (base %s %s)\n", wt.ID, wt.Path, wt.BaseRef, wt.BaseSHA)
					return err
				default:
					_, err := fmt.Fprintf(w, "Adopted worktree %s at %s from %s %s\n", wt.ID, wt.Path, wt.BaseRef, wt.BaseSHA)
					return err
				}
			}, out)
		},
	}
	cmd.Flags().StringVar(&projectFlag, "project", "", "namespace path of the adopted project this worktree belongs to (required when it cannot be inferred uniquely)")
	cmd.Flags().StringVar(&baseRefFlag, "base-ref", "", "explicit base ref (e.g. origin/gh-pages) instead of the resolved default branch")
	cmd.Flags().BoolVar(&allowShallow, "allow-shallow", false, "adopt even though the repository is a shallow clone (recorded base_sha may be inaccurate)")
	return cmd
}

// projectForWorktreeAdopt maps a linked worktree's main checkout to the one
// adopted project it belongs to. `--project` DISAMBIGUATES; it does not
// override — the named project's own checkout must still be this worktree's
// main checkout, or the row would record provenance for a repo the worktree
// does not belong to (and `remove --prune` would later run `git worktree
// remove` from the wrong repository). Without an explicit flag, every
// project's local checkout path is compared against mainCheckout — resolved
// identically, so a /var vs /private/var spelling difference cannot hide a
// real match — refusing with a usage error naming every candidate when the
// match is missing or ambiguous, the same shape `wip show`'s resolveWipTarget
// uses for its multi-candidate refusal.
func projectForWorktreeAdopt(ctx context.Context, opts *options, store *state.Store, mainCheckout, projectFlag string) (state.ProjectStatus, error) {
	resolvedCheckout := func(p state.ProjectStatus) string {
		repoPath := p.LocalPath
		if repoPath == "" {
			repoPath = filepath.Join(opts.paths().Root, filepath.FromSlash(p.Path))
		}
		resolved, rerr := filepath.EvalSymlinks(repoPath)
		if rerr != nil {
			resolved = filepath.Clean(repoPath)
		}
		return resolved
	}
	if projectFlag != "" {
		p, err := store.ProjectByPath(ctx, projectFlag)
		if err != nil {
			return state.ProjectStatus{}, err
		}
		if got := resolvedCheckout(p); got != mainCheckout {
			return state.ProjectStatus{}, appError{code: exitUsage, err: fmt.Errorf("--project %s is checked out at %s, but that worktree belongs to %s; --project disambiguates between matching projects, it cannot reassign a worktree to a different repository", projectFlag, got, mainCheckout)}
		}
		return p, nil
	}
	projects, err := store.ListProjects(ctx)
	if err != nil {
		return state.ProjectStatus{}, err
	}
	var matches []state.ProjectStatus
	for _, p := range projects {
		if p.Type != "git_repo" {
			continue
		}
		if resolvedCheckout(p) == mainCheckout {
			matches = append(matches, p)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return state.ProjectStatus{}, appError{code: exitUsage, err: fmt.Errorf("no adopted project's local checkout matches %s; pass --project <namespace-path>", mainCheckout)}
	default:
		names := make([]string, 0, len(matches))
		for _, p := range matches {
			names = append(names, p.Path)
		}
		return state.ProjectStatus{}, appError{code: exitUsage, err: fmt.Errorf("multiple adopted projects match %s (%s); pick one with --project", mainCheckout, strings.Join(names, ", "))}
	}
}

func applyWorktreeLFSPolicy(ctx context.Context, warn io.Writer, r dsgit.Runner, project state.ProjectStatus, wtPath string) error {
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
		_, _ = fmt.Fprintf(warn, "warning: %s uses Git LFS; worktree %s may contain pointer files (lfs_policy=%s)\n", project.Path, wtPath, policy)
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
	SchemaVersion int    `json:"schema_version"`
	ID            string `json:"id"`
	Path          string `json:"path"`
	Branch        string `json:"branch"`
	BaseRef       string `json:"base_ref"`
	BaseSHA       string `json:"base_sha"`
	CurrentSHA    string `json:"current_sha"`
	Fresh         bool   `json:"fresh"`
	Behind        int    `json:"behind"`
	DirtyState    string `json:"dirty_state"`
}

// worktreeStatusSchemaVersion is the contract version for
// worktreeStatusOutput. Bump ONLY for an additive change; a field rename or
// removal is a breaking change that needs a deliberate decision, not a bump
// (spec/13 § Machine contract surfaces).
const worktreeStatusSchemaVersion = 1

// statusWorktree resolves a worktree row and grades it against its recorded
// upstream base. It is the whole of `worktree status`'s domain logic, split
// out of the cobra RunE so a non-CLI caller (the AD5-07 MCP server) invokes
// the SAME function rather than a reimplementation — the RunE below keeps
// only store lifecycle and rendering.
//
// A DirtyState probe failure is deliberately NOT an error: a worktree whose
// dirtiness cannot be determined still has a meaningful freshness verdict, so
// the state degrades to "unknown" exactly as it did inline.
func statusWorktree(ctx context.Context, opts *options, store *state.Store, worktreeID string) (worktreeStatusOutput, error) {
	wt, err := store.WorktreeByID(ctx, worktreeID)
	if err != nil {
		return worktreeStatusOutput{}, err
	}
	r := gitRunner(opts)
	drift, err := r.BaseDrift(ctx, wt.Path, wt.BaseRef, wt.BaseSHA)
	if err != nil {
		return worktreeStatusOutput{}, appError{code: exitGit, err: err}
	}
	dirty, err := r.DirtyState(ctx, wt.Path)
	if err != nil {
		dirty = dsgit.DirtyUnknown
	}
	return worktreeStatusOutput{
		SchemaVersion: worktreeStatusSchemaVersion,
		ID:            wt.ID,
		Path:          wt.Path,
		Branch:        wt.Branch,
		BaseRef:       wt.BaseRef,
		BaseSHA:       wt.BaseSHA,
		CurrentSHA:    drift.CurrentSHA,
		Fresh:         drift.Fresh,
		Behind:        drift.Behind,
		DirtyState:    string(dirty),
	}, nil
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
			out, err := statusWorktree(cmd.Context(), opts, store, args[0])
			if err != nil {
				return err
			}
			return opts.render(stdout, func(w io.Writer) error {
				status := "fresh"
				if !out.Fresh {
					status = fmt.Sprintf("stale (behind %d)", out.Behind)
				}
				_, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", out.ID, status, out.BaseRef, out.CurrentSHA, out.DirtyState)
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

// listWorktrees returns every active worktree row. It is a thin pass-through
// today, and exists as a named function only so the AD5-07 MCP server calls
// the same entry point the CLI does instead of reaching into the store on its
// own — the "no second execution path" rule holds even where the path is one
// line long, because that is precisely where a divergent reimplementation is
// cheapest to write.
//
// The return type is []state.Worktree, NOT a versioned envelope: `worktree
// list --json` has always emitted a bare JSON array, and wrapping it in an
// object would be a breaking shape change rather than the additive evolution
// spec/13 § Machine contract surfaces permits. An MCP tool that wants an
// envelope wraps this result itself, for its own separate consumer.
func listWorktrees(ctx context.Context, store *state.Store) ([]state.Worktree, error) {
	return store.ListWorktrees(ctx)
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
			worktrees, err := listWorktrees(cmd.Context(), store)
			if err != nil {
				return err
			}
			return opts.render(stdout, func(w io.Writer) error {
				for _, wt := range worktrees {
					// AD5-06: human output shows provenance, because a user
					// cannot otherwise tell which worktrees DevStrap created
					// from those it merely adopted — and the two have different
					// reap semantics (`cleanup --merged` skips adopted rows
					// unless --include-adopted, `remove` deregisters rather than
					// deleting). The --json branch needs no change: it encodes
					// state.Worktree directly, which has always carried
					// created_by.
					branch := wt.Branch
					if branch == "" {
						// An adopted detached-HEAD worktree stores "" (the
						// contract --json consumers see). Rendering an empty
						// column as blank reads as a bug; label it for humans
						// only.
						branch = "(detached)"
					}
					_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", wt.ID, wt.CreatedBy, branch, wt.BaseRef, wt.Path)
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
	var prune bool
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
			// An adopted worktree is a registration of a checkout DevStrap did
			// not create. Reaping it the same way as a `worktree new` checkout
			// would delete something devstrap does not own; deregister only
			// (mark the row removed, leave the checkout on disk) unless the
			// caller explicitly opts into also removing the physical checkout.
			if wt.CreatedBy == "adopted" && !prune {
				if err := store.MarkWorktreeRemoved(cmd.Context(), args[0]); err != nil {
					return err
				}
				out := worktreeRemoveResult{ID: args[0], Pruned: false}
				return opts.render(stdout, func(w io.Writer) error {
					_, err := fmt.Fprintf(w, "Deregistered adopted worktree %s; checkout left in place at %s (pass --prune to also remove it)\n", out.ID, wt.Path)
					return err
				}, out)
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
	cmd.Flags().BoolVar(&prune, "prune", false, "for an adopted worktree, also remove the physical checkout (by default only the registration is removed)")
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
	var includeAdopted bool
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
				// Adopted worktrees are registrations of checkouts DevStrap did
				// not create; `cleanup --merged` must never `git branch -D`
				// something it did not create without an explicit opt-in.
				if wt.CreatedBy == "adopted" && !includeAdopted {
					skipped++
					continue
				}
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
	cmd.Flags().BoolVar(&includeAdopted, "include-adopted", false, "also consider adopted worktrees for reaping (by default they are skipped)")
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
	// A detached-HEAD worktree (Branch == "", the common shape for an adopted
	// worktree) is NEVER merge-eligible. Without this guard,
	// strings.Contains(mergedOut, wt.Branch) below is true for ANY mergedOut
	// when wt.Branch == "" (strings.Contains(x, "") is always true in Go), so
	// every clean detached worktree would read as "merged" the moment
	// --include-adopted lets it reach this point — reaping exactly the
	// checkouts that flag exists to protect, in exactly the detached case
	// that is the common one (AD5-02 review finding).
	if wt.Branch == "" {
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
		// P6 path-normalization parity (AD5-02): resolve symlinks the same way
		// `worktree adopt` does, so the SAME physical worktree gets the SAME
		// stored path (idx_worktrees_active_path, migration 00032) regardless
		// of which command registered it — otherwise a macOS /var vs
		// /private/var alias lets the unique index miss a real duplicate.
		// EvalSymlinks can fail on an unusual filesystem even though the
		// worktree was just created; fall back to the unresolved path rather
		// than losing the registration over a cosmetic normalization step.
		if resolved, rerr := filepath.EvalSymlinks(wtPath); rerr == nil {
			wtPath = resolved
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
