package cli

// devstrap promote <path> --draft | --git-remote <url> (NOVCS-03).
//
// The namespace carries four project types (spec/07): `git_repo` (a validated
// remote, blobless-cloned on materialize), `local_git` (a real git repo with no
// usable remote, NOVCS-01), `draft_project` (non-git content synced as an
// age-encrypted bundle), and `plain_folder` (structure only). Until this
// command, a remote-less folder had no way to graduate: the only route to
// `git_repo` was re-adopting through `devstrap add`.
//
// `local_git` is the primary case, and the one an implementation is most
// likely to get wrong: it is a git repository the user simply never pushed —
// the "forgot to push" population this product exists for — so it must be
// promoted by ADDING a remote and PUSHING its existing history. Running
// `git init` over it would destroy that history. The type switch below is the
// only place that distinction lives, and internal/git/promote.go deliberately
// exposes no primitive that could initialize over an existing repo.
//
// Demotion (git_repo -> anything) is out of scope: a promotion is a
// one-directional graduation, and the refusal that enforces it is load-bearing
// for the event design (see refuseAlreadyRemote).

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	dsgit "github.com/Reederey87/DevStrap/internal/git"
	"github.com/Reederey87/DevStrap/internal/ignore"
	"github.com/Reederey87/DevStrap/internal/pathkey"
	"github.com/Reederey87/DevStrap/internal/redact"
	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
	"github.com/spf13/cobra"
)

// promoteInitialCommitMessage is the message of the single commit `promote`
// creates when a non-git folder becomes a repository. It never rewrites or
// amends anything: a `local_git` promotion creates no commit at all.
const promoteInitialCommitMessage = "Initial commit (devstrap promote)"

// promoteResult is the --json shape for `devstrap promote` (P5-CLI-01 part B).
type promoteResult struct {
	Path     string `json:"path"`
	FromType string `json:"from_type"`
	ToType   string `json:"to_type"`
	Remote   string `json:"remote,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Pushed   bool   `json:"pushed"`
	// Changed is false when the project already had the requested type, so a
	// script can tell an idempotent re-run from a real transition.
	Changed bool `json:"changed"`
}

func newPromoteCommand(stdout io.Writer, opts *options) *cobra.Command {
	var draft bool
	var gitRemote string
	cmd := &cobra.Command{
		Use:   "promote <path>",
		Short: "Graduate a remote-less folder into a draft project or a real Git repository",
		Long: "Promote a remote-less namespace entry.\n\n" +
			"  --draft              plain_folder -> draft_project (content syncs as an encrypted bundle)\n" +
			"  --git-remote <url>   local_git -> git_repo (pushes the EXISTING history), or\n" +
			"                       plain_folder/draft_project -> git_repo (git init, initial commit, push)\n\n" +
			"The remote must exist and be empty; a remote that already holds refs is a\n" +
			"`devstrap add` case, not a promotion. Demotion is not supported.",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case draft && gitRemote != "":
				return appError{code: exitUsage, err: fmt.Errorf("--draft and --git-remote are mutually exclusive")}
			case !draft && gitRemote == "":
				return appError{code: exitUsage, err: fmt.Errorf("one of --draft or --git-remote <url> is required")}
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
			// The FIRST gate for both directions: see refuseAlreadyRemote.
			if err := refuseAlreadyRemote(project); err != nil {
				return err
			}
			if draft {
				return promoteToDraft(cmd.Context(), stdout, store, opts, project)
			}
			return promoteToGitRepo(cmd.Context(), stdout, cmd.ErrOrStderr(), store, opts, project, gitRemote)
		},
	}
	cmd.Flags().BoolVar(&draft, "draft", false, "promote a plain folder to a draft project (content syncs as an encrypted bundle)")
	cmd.Flags().StringVar(&gitRemote, "git-remote", "", "promote to a git_repo by pushing to this empty remote URL")
	return cmd
}

// refuseAlreadyRemote rejects promoting a project that already carries a
// remote.
//
// This refusal is LOAD-BEARING, not tidiness. `promote` reuses the existing
// `project.updated` event rather than introducing a new event kind, and that is
// only safe because of the guard in internal/sync/decide.go's decideUpsert:
//
//	if hasActive && existing.RemoteKey != "" && payload.RemoteKey != "" &&
//	   existing.RemoteKey != payload.RemoteKey {
//
// The same-path/different-remote reconciliation branch requires a non-empty
// EXISTING remote key. Every legitimate promotion source (local_git,
// draft_project, plain_folder) has `RemoteKey == ""`, so a promotion event
// falls through to plain HLC last-writer-wins and can never raise a spurious
// fleet-wide conflict. A project that already has a remote is precisely the
// case that WOULD reach that branch — so it is refused here rather than
// announced. `promote_test.go` and the convergence testscript both pin this.
func refuseAlreadyRemote(project state.ProjectStatus) error {
	if project.Type != "git_repo" && project.RemoteKey == "" {
		return nil
	}
	detail := fmt.Sprintf("%s is already %s", project.Path, project.Type)
	if remote := redact.StripURLUserinfo(project.RemoteURL); remote != "" {
		detail += fmt.Sprintf(" tracking %s", remote)
	}
	return appError{code: exitInvalidConfig, err: fmt.Errorf(
		"%s; promote only graduates remote-less projects — use `devstrap add --path %s <remote>` to track a different remote (demotion is out of scope)",
		detail, project.Path)}
}

// promoteLocalPath resolves and re-validates the on-disk directory backing a
// project. LocalPath is empty for scan-adopted rows (Tx.UpsertProject does not
// write device_project_state), so it falls back to root/<path> exactly as
// `draft snapshot create` does; VerifyWithinRoot is the same use-time symlink
// re-validation the materialization path performs (SEC-4).
func promoteLocalPath(opts *options, project state.ProjectStatus) (string, error) {
	localPath := project.LocalPath
	if localPath == "" {
		localPath = filepath.Join(opts.paths().Root, filepath.FromSlash(project.Path))
	}
	if root := opts.paths().Root; root != "" {
		if err := pathkey.VerifyWithinRoot(root, localPath); err != nil {
			return "", appError{code: exitInvalidConfig, err: fmt.Errorf("refusing to promote outside managed root: %w", err)}
		}
	}
	info, err := os.Stat(localPath)
	if err != nil {
		return "", appError{code: exitInvalidConfig, err: fmt.Errorf("%s is not present on disk at %s: %w", project.Path, localPath, err)}
	}
	if !info.IsDir() {
		return "", appError{code: exitInvalidConfig, err: fmt.Errorf("%s is not a directory at %s", project.Path, localPath)}
	}
	return localPath, nil
}

func promoteToDraft(ctx context.Context, stdout io.Writer, store *state.Store, opts *options, project state.ProjectStatus) error {
	if project.Type == "draft_project" {
		// Idempotent no-op: nothing is written and no event is emitted, so a
		// re-run cannot churn the namespace map.
		return opts.render(stdout, func(w io.Writer) error {
			_, err := fmt.Fprintf(w, "%s is already draft_project; nothing to do\n", project.Path)
			return err
		}, promoteResult{Path: project.Path, FromType: project.Type, ToType: project.Type})
	}
	if project.Type != "plain_folder" {
		// local_git lands here. It already syncs through the encrypted bundle
		// plane, so --draft would buy nothing, and bundling a directory that
		// holds a .git is the one thing the ignore compiler exists to prevent.
		return appError{code: exitInvalidConfig, err: fmt.Errorf(
			"%s is %s; --draft promotes a plain_folder only (a %s already syncs as an encrypted bundle) — use --git-remote <url> to promote it to a git_repo",
			project.Path, project.Type, project.Type)}
	}
	localPath, err := promoteLocalPath(opts, project)
	if err != nil {
		return err
	}
	if dsgit.IsRepo(localPath) {
		// The row says plain_folder but the directory is a git repository —
		// stale state, and draft-bundling it would ship a .git through the blob
		// plane. Refuse rather than reclassify behind the user's back.
		return appError{code: exitInvalidConfig, err: fmt.Errorf(
			"%s is recorded plain_folder but %s is a git repository; re-run `devstrap scan --adopt` so it is reclassified, then promote with --git-remote <url>",
			project.Path, localPath)}
	}
	if err := recordPromotion(ctx, store, promotionRecord{
		Path:      project.Path,
		Type:      "draft_project",
		LocalPath: localPath,
	}); err != nil {
		return err
	}
	return opts.render(stdout, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "Promoted %s: %s -> draft_project (run `devstrap draft snapshot create %s` to sync its content)\n",
			project.Path, project.Type, project.Path)
		return err
	}, promoteResult{Path: project.Path, FromType: project.Type, ToType: "draft_project", Changed: true})
}

func promoteToGitRepo(ctx context.Context, stdout, stderr io.Writer, store *state.Store, opts *options, project state.ProjectStatus, remote string) error {
	// Route the URL through the SAME validated-remote helper `scan` and `add`
	// use, so the protocol allowlist and userinfo stripping apply identically —
	// a second validator would be a second policy.
	remoteKey, err := dsgit.CanonicalRemoteKey(remote)
	if err != nil {
		return appError{code: exitInvalidConfig, err: err}
	}
	localPath, err := promoteLocalPath(opts, project)
	if err != nil {
		return err
	}
	// Hold the project's repo operation lock for the whole git-mutating
	// stretch, exactly as hydrate/materialize/worktree do. Promotion runs
	// `init`/`remote add`/`push` against a managed working tree, so a
	// concurrent convergence tick materializing the same project would
	// otherwise interleave with it.
	unlock, err := acquireRepoLock(opts.paths().Home, project.ID)
	if err != nil {
		return err
	}
	defer unlock()

	r := gitRunner(opts)
	safeRemote := redact.StripURLUserinfo(remote)

	// Preflight before ANY local mutation: a remote that already holds refs is
	// an `add` case. Only an EMPTY remote may receive a promotion push, so a
	// promotion can never leave two unrelated histories in one repository.
	empty, err := r.RemoteIsEmpty(ctx, remote)
	if err != nil {
		return appError{code: exitGit, err: fmt.Errorf(
			"could not read %s (create the empty remote repository first): %w", safeRemote, err)}
	}
	if !empty {
		return appError{code: exitInvalidConfig, err: fmt.Errorf(
			"%s already has refs; promote pushes into an EMPTY remote only — use `devstrap add --path %s %s` to track an existing repository",
			safeRemote, project.Path, safeRemote)}
	}

	// rollback restores the working tree to its exact pre-command state. It is
	// what makes "a failed push leaves the project at its original type" true on
	// disk as well as in the database.
	var rollback func()
	branch := ""
	switch project.Type {
	case "local_git":
		// THE primary case. This branch must never call InitRepo: the whole
		// point is that the user's existing commits reach the new remote.
		if !dsgit.IsRepo(localPath) {
			return appError{code: exitInvalidConfig, err: fmt.Errorf(
				"%s is recorded local_git but %s is not a git repository; re-run `devstrap scan --adopt`", project.Path, localPath)}
		}
		if existing, err := r.RemoteURL(ctx, localPath); err == nil && strings.TrimSpace(existing) != "" {
			// A local_git can be classified so because its origin FAILED
			// validation (scan.go, NOVCS-01), not only because it has none.
			// Silently rewriting that origin would discard a remote the user
			// configured; refuse and name it.
			return appError{code: exitInvalidConfig, err: fmt.Errorf(
				"%s already has an 'origin' remote (%s); promote will not rewrite it — fix or remove it with `git remote remove origin` first",
				project.Path, redact.StripURLUserinfo(existing))}
		}
		if !r.HasCommits(ctx, localPath) {
			return appError{code: exitInvalidConfig, err: fmt.Errorf(
				"%s has no commits yet; commit something before promoting (a promotion pushes existing history, it never invents one)", project.Path)}
		}
		branch, err = r.CurrentBranch(ctx, localPath)
		if err != nil {
			return appError{code: exitGit, err: fmt.Errorf(
				"%s has a detached HEAD; check out a branch before promoting: %w", project.Path, err)}
		}
	case "plain_folder", "draft_project":
		if dsgit.IsRepo(localPath) {
			return appError{code: exitInvalidConfig, err: fmt.Errorf(
				"%s is recorded %s but %s is a git repository; re-run `devstrap scan --adopt` so it is reclassified as local_git first",
				project.Path, project.Type, localPath)}
		}
		branch = project.DefaultBranch
		if branch == "" || !dsgit.SafeBranchName(branch) {
			branch = "main"
		}
		created, err := promoteInitRepo(ctx, r, project, localPath, branch)
		if err != nil {
			return err
		}
		rollback = created
	default:
		return appError{code: exitInvalidConfig, err: fmt.Errorf(
			"%s is %s; promote --git-remote handles local_git, plain_folder, and draft_project", project.Path, project.Type)}
	}

	if err := r.AddRemote(ctx, localPath, "origin", remote); err != nil {
		promoteRollback(rollback)
		return appError{code: exitGit, err: err}
	}
	if rollback == nil {
		// local_git: the only thing this command changed so far is the origin
		// it just added, so undoing that restores the repo exactly.
		// A silent rollback failure is worse than the failure it follows: the
		// user is told the tree is back at its pre-command state when it is
		// not, and a retry then hits the "already has an 'origin'" refusal,
		// which wrongly implies they configured it themselves.
		rollback = func() {
			if rmErr := r.RemoveRemote(context.WithoutCancel(ctx), localPath, "origin"); rmErr != nil {
				_, _ = fmt.Fprintf(stderr, "warning: could not remove the 'origin' this attempt added to %s; remove it manually before retrying (git -C %s remote remove origin): %s\n",
					project.Path, localPath, redact.Scrub(rmErr.Error()))
			}
		}
	}
	if err := r.PushBranch(ctx, localPath, "origin", branch); err != nil {
		promoteRollback(rollback)
		return appError{code: exitGit, err: fmt.Errorf("push %s to %s: %w", branch, safeRemote, err)}
	}

	// Only now is the project a git_repo. Order matters: validate -> push ->
	// record. Recording first would publish a git_repo pointing at a remote
	// holding no commits, which every other device would then try to clone.
	if err := recordPromotion(ctx, store, promotionRecord{
		Path:          project.Path,
		Type:          "git_repo",
		RemoteURL:     remote,
		RemoteKey:     remoteKey,
		DefaultBranch: branch,
		LocalPath:     localPath,
	}); err != nil {
		// The push already succeeded, so the remote is populated while the row
		// is not promoted. Deliberately NOT rolled back: deleting a remote
		// branch to tidy up local bookkeeping risks destroying the only copy of
		// the history.
		//
		// The remedy is `scan --adopt`, NOT `add`. `add` calls
		// ensureHydratableTarget (add.go:73), which refuses a non-empty,
		// non-skeleton directory — and in exactly this state the directory is
		// the just-promoted repository, full of the user's files. Naming `add`
		// here would send the user to a command that fails in precisely the
		// state this error leaves behind. `scan --adopt` re-adopts an existing
		// checkout whose origin validates, which is what this is.
		return fmt.Errorf("%s was pushed to %s but recording it failed; re-run `devstrap scan --adopt` to adopt the now-pushed repository: %w",
			project.Path, safeRemote, err)
	}
	return opts.render(stdout, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "Promoted %s: %s -> git_repo (pushed %s to %s)\n", project.Path, project.Type, branch, safeRemote)
		return err
	}, promoteResult{
		Path:     project.Path,
		FromType: project.Type,
		ToType:   "git_repo",
		Remote:   safeRemote,
		Branch:   branch,
		Pushed:   true,
		Changed:  true,
	})
}

// promoteInitRepo turns a non-git folder into a repository with exactly one
// commit and returns the rollback that removes it again. It refuses an empty
// folder rather than inventing an empty commit, and refuses to commit
// secret-looking files rather than pushing them to a remote.
func promoteInitRepo(ctx context.Context, r dsgit.Runner, project state.ProjectStatus, localPath, branch string) (func(), error) {
	entries, err := os.ReadDir(localPath)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, appError{code: exitInvalidConfig, err: fmt.Errorf(
			"%s is empty; put something in it before promoting (promote will not create an empty initial commit)", project.Path)}
	}
	if err := r.InitRepo(ctx, localPath, branch); err != nil {
		return nil, appError{code: exitGit, err: err}
	}
	// Everything below created .git, so removing it restores the folder exactly
	// — the user's files are never touched by any of it.
	rollback := func() { _ = os.RemoveAll(filepath.Join(localPath, ".git")) }
	if err := r.StageAll(ctx, localPath); err != nil {
		rollback()
		return nil, appError{code: exitGit, err: err}
	}
	staged, err := r.StagedFiles(ctx, localPath)
	if err != nil {
		rollback()
		return nil, appError{code: exitGit, err: err}
	}
	if len(staged) == 0 {
		rollback()
		return nil, appError{code: exitInvalidConfig, err: fmt.Errorf(
			"%s has nothing to commit (every file is git-ignored); promote will not create an empty initial commit", project.Path)}
	}
	// Screen what would actually be COMMITTED (the index, so .gitignore is
	// already honored) rather than what is merely on disk. A promotion push is
	// the first time this content leaves the machine, and `scan` already treats
	// these filenames as a warning class.
	if secrets := secretLookingStagedFiles(staged); len(secrets) > 0 {
		rollback()
		return nil, appError{code: exitInvalidConfig, err: fmt.Errorf(
			"%s would commit secret-looking file(s) to a remote: %s; remove or .gitignore them first",
			project.Path, strings.Join(secrets, ", "))}
	}
	if err := r.CommitStaged(ctx, localPath, promoteInitialCommitMessage); err != nil {
		rollback()
		return nil, appError{code: exitGit, err: err}
	}
	return rollback, nil
}

// secretLookingStagedFilesLimit bounds how many names the refusal lists; the
// count is what matters once it is long.
const secretLookingStagedFilesLimit = 5

func secretLookingStagedFiles(staged []string) []string {
	var found []string
	for _, name := range staged {
		if ignore.IsSecretPath(filepath.ToSlash(name)) {
			found = append(found, name)
		}
	}
	sort.Strings(found)
	if len(found) > secretLookingStagedFilesLimit {
		extra := len(found) - secretLookingStagedFilesLimit
		found = append(found[:secretLookingStagedFilesLimit:secretLookingStagedFilesLimit],
			fmt.Sprintf("and %d more", extra))
	}
	return found
}

// promoteRollback runs a rollback step if one has been armed yet. The steps
// themselves run on an uncancellable context (see the local_git arm), so an
// interrupted promotion still restores the working tree.
func promoteRollback(rollback func()) {
	if rollback != nil {
		rollback()
	}
}

type promotionRecord struct {
	Path          string
	Type          string
	RemoteURL     string
	RemoteKey     string
	DefaultBranch string
	LocalPath     string
}

// recordPromotion emits the namespace event and updates the local row in ONE
// transaction, using the EXISTING project.updated event kind (see
// refuseAlreadyRemote for why that is safe). MaterializationState is
// "available" because the content is, by construction, already on disk here —
// recording "skeleton" would invite the eager materializer to clone over it.
func recordPromotion(ctx context.Context, store *state.Store, rec promotionRecord) error {
	return store.WithTx(ctx, func(tx *state.Tx) error {
		event, err := dssync.CreateProjectEventTx(ctx, store, tx, dssync.EventProjectUpdated, dssync.ProjectPayload{
			Path:          rec.Path,
			Type:          rec.Type,
			RemoteURL:     rec.RemoteURL,
			RemoteKey:     rec.RemoteKey,
			DefaultBranch: rec.DefaultBranch,
		})
		if err != nil {
			return err
		}
		_, err = tx.UpsertProject(ctx, state.UpsertProjectParams{
			Path:                  rec.Path,
			Type:                  rec.Type,
			RemoteURL:             rec.RemoteURL,
			RemoteKey:             rec.RemoteKey,
			DefaultBranch:         rec.DefaultBranch,
			MaterializationPolicy: "lazy",
			LocalPath:             rec.LocalPath,
			MaterializationState:  "available",
			DirtyState:            "unknown",
			SourceEventHLC:        event.HLC,
			SourceEventDeviceID:   event.DeviceID,
			SourceEventID:         event.ID,
		})
		return err
	})
}
