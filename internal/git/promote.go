package git

// Primitives backing `devstrap promote --git-remote` (NOVCS-03): graduating a
// remote-less folder or a remote-less git repo into a real `git_repo`.
//
// The load-bearing property of this set is what it does NOT contain: there is
// no "initialize over an existing repository" primitive. A `local_git` — a git
// repo the user never pushed — is promoted by ADDING a remote and PUSHING its
// existing history (AddRemote + PushBranch); running InitRepo over it would
// destroy exactly the history the promotion exists to rescue. InitRepo is only
// ever reached from a `plain_folder`/`draft_project`, which by definition has
// no `.git` at all.

import (
	"context"
	"fmt"
	"strings"
)

// RemoteIsEmpty reports whether remote advertises no refs at all.
//
// It is the preflight for a promotion push: `spec/00`'s core promise is that
// remote-less folders are "never adopted as broken clonable git repos", and
// the mirror of that is never pushing an unrelated history into a remote that
// already holds one. A remote with refs means the user wants `devstrap add`,
// not `promote`. An unreachable/nonexistent remote returns an error rather
// than "empty" — a remote that cannot be read has not been proven empty.
func (r Runner) RemoteIsEmpty(ctx context.Context, remote string) (bool, error) {
	if err := ValidateRemote(remote); err != nil {
		return false, err
	}
	ctx, cancel := r.longTransferContext(ctx)
	defer cancel()
	// `--` terminates options so a remote that survived ValidateRemote but
	// still reads option-like to ls-remote cannot become a flag.
	out, err := r.Run(ctx, "", "ls-remote", "--", remote)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "", nil
}

// InitRepo runs `git init -b <branch>` in dir. Callers MUST have established
// that dir is not already a repository: `git init` over an existing repo is a
// documented no-op that silently keeps the old HEAD, so a caller that skipped
// the check would believe it created the branch it asked for.
func (r Runner) InitRepo(ctx context.Context, dir, branch string) error {
	if !safeBranchName(branch) {
		return fmt.Errorf("invalid git branch name %q", branch)
	}
	_, err := r.Run(ctx, dir, "init", "-b", branch)
	return err
}

// StageAll stages every non-ignored change in dir (`git add -A`).
func (r Runner) StageAll(ctx context.Context, dir string) error {
	_, err := r.Run(ctx, dir, "add", "-A")
	return err
}

// GitlinkMode is the index mode `git add -A` records for a nested git
// repository: a commit hash with no objects behind it in THIS repository.
const GitlinkMode = "160000"

// StagedFile is one index entry: its mode and its path.
type StagedFile struct {
	Mode string
	Path string
}

// StagedFiles lists the index contents of dir.
//
// It reads `git ls-files`, not `git diff --cached`, because the caller's
// repository has an UNBORN HEAD (nothing has been committed yet) and a diff
// against a HEAD that does not exist is not a stable contract. `-z` keeps
// filenames holding newlines intact.
//
// `--stage` rather than `--cached` because the MODE is load-bearing, not
// decoration: `git add -A` records a nested repository as a gitlink
// (GitlinkMode) instead of descending into it, git's own
// "warning: adding embedded git repository" goes to stderr where Run drops it,
// and the caller must be able to refuse rather than push a commit referencing
// objects the remote will never receive.
//
// The output is `<mode> <sha> <stage>\t<path>` per record. Split on \x00 first
// (that is the record separator `-z` gives, and it is the only byte a path
// cannot contain), then on the FIRST \t — a path may itself contain tabs, but
// the metadata prefix before the first one never does.
func (r Runner) StagedFiles(ctx context.Context, dir string) ([]StagedFile, error) {
	out, err := r.Run(ctx, dir, "ls-files", "-z", "--stage")
	if err != nil {
		return nil, err
	}
	var files []StagedFile
	for _, record := range strings.Split(out, "\x00") {
		if record == "" {
			continue
		}
		meta, path, ok := strings.Cut(record, "\t")
		if !ok {
			return nil, fmt.Errorf("unparsable `git ls-files --stage` record %q", record)
		}
		fields := strings.Fields(meta)
		if len(fields) != 3 {
			return nil, fmt.Errorf("unparsable `git ls-files --stage` record %q", record)
		}
		files = append(files, StagedFile{Mode: fields[0], Path: path})
	}
	return files, nil
}

// CommitStaged commits the index with an explicit inline identity.
//
// The identity is supplied per-invocation rather than read from the user's
// config because Runner deliberately runs every git subprocess with
// GIT_CONFIG_GLOBAL=/dev/null and GIT_CONFIG_NOSYSTEM=1 (gitEnv), so there is
// no user identity to inherit and git's hostname auto-detection is not
// dependable. This mirrors the git-carrier hub's own commits.
func (r Runner) CommitStaged(ctx context.Context, dir, message string) error {
	_, err := r.Run(ctx, dir,
		"-c", "user.name=devstrap", "-c", "user.email=devstrap@localhost",
		"commit", "--quiet", "-m", message)
	return err
}

// AddRemote configures a named remote in dir. The URL passes the same
// ValidateRemote gate every other remote-consuming primitive uses, so the
// protocol allowlist applies identically here.
func (r Runner) AddRemote(ctx context.Context, dir, name, url string) error {
	if !safeRemoteName(name) {
		return fmt.Errorf("invalid git remote name %q", name)
	}
	if err := ValidateRemote(url); err != nil {
		return err
	}
	_, err := r.Run(ctx, dir, "remote", "add", name, url)
	return err
}

// RemoveRemote drops a named remote from dir. Used to roll a promotion back to
// its exact pre-command state when the push fails.
func (r Runner) RemoveRemote(ctx context.Context, dir, name string) error {
	if !safeRemoteName(name) {
		return fmt.Errorf("invalid git remote name %q", name)
	}
	_, err := r.Run(ctx, dir, "remote", "remove", name)
	return err
}

// CurrentBranch returns the checked-out branch name via
// `git symbolic-ref --short HEAD`. That form is chosen over
// `rev-parse --abbrev-ref HEAD` for two reasons: it answers correctly for an
// UNBORN branch (a fresh `git init` with no commits, where rev-parse fails),
// and it fails cleanly on a detached HEAD instead of returning the literal
// string "HEAD" that a caller must then special-case.
func (r Runner) CurrentBranch(ctx context.Context, dir string) (string, error) {
	out, err := r.Run(ctx, dir, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// HasCommits reports whether dir's HEAD resolves to a commit. A `git init`
// with nothing committed has none, and there is then no history to push.
func (r Runner) HasCommits(ctx context.Context, dir string) bool {
	_, err := r.Run(ctx, dir, "rev-parse", "--verify", "--quiet", "HEAD")
	return err == nil
}
