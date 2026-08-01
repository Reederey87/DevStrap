package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Reederey87/DevStrap/internal/childenv"
)

type Runner struct {
	Bin     string
	Timeout time.Duration
	// LongTimeout is the per-attempt deadline for network-transfer commands
	// that legitimately run for tens of minutes on large repositories: clone,
	// fetch, and git lfs pull. Other git commands use Timeout.
	LongTimeout   time.Duration
	RetryAttempts int
	RetryBackoff  time.Duration
	// RetryCap bounds the per-sleep backoff so exponential growth cannot exceed
	// a sane ceiling (QUAL-06).
	RetryCap time.Duration
	// MaxElapsed bounds the total wall-clock time of a single operation's
	// retry loop (across all attempts). Zero means no aggregate budget (bounded
	// only by RetryAttempts and the per-command Timeout). Set by callers that
	// need a hung operation to fail fast instead of wedging a worker slot
	// (QUAL-06).
	MaxElapsed time.Duration
}

func NewRunner() Runner {
	return Runner{
		Bin:           "git",
		Timeout:       2 * time.Minute,
		LongTimeout:   30 * time.Minute,
		RetryAttempts: 3,
		RetryBackoff:  200 * time.Millisecond,
		RetryCap:      5 * time.Second,
	}
}

var (
	ErrNetwork        = errors.New("git network error")
	ErrTimeout        = errors.New("git timeout")
	ErrAuth           = errors.New("git authentication error")
	ErrBranchNotFound = errors.New("git branch not found")
	ErrRemoteMissing  = errors.New("git remote missing")
	// ErrNonFastForward classifies a push rejected because the remote ref
	// advanced past the local view (someone else pushed first). It is the
	// retryable outcome of the git-carrier hub's optimistic write loop:
	// refetch, re-apply, push again — never a terminal failure.
	ErrNonFastForward = errors.New("git non-fast-forward push")
	// ErrNoMergeBase classifies `git merge-base`'s documented exit-1/empty-output
	// outcome: the two refs share no common ancestor (an orphan branch such as
	// gh-pages is the common case). This is an EXPECTED result, never an
	// operational failure — callers such as `worktree adopt` must surface it as
	// a usage refusal naming an explicit --base-ref, not as a bare error.
	ErrNoMergeBase = errors.New("git no common ancestor")
)

type CommandError struct {
	Kind    error
	Args    string
	Message string
	Code    int
}

func (e CommandError) Error() string {
	if e.Message == "" {
		return "git " + e.Args
	}
	return "git " + e.Args + ": " + e.Message
}

func (e CommandError) Unwrap() error {
	return e.Kind
}

// ExitCode returns the git subprocess exit status, or -1 when the process did
// not report one (for example when it could not be started). Callers that rely
// on git's documented tri-state commands must distinguish an expected status
// such as merge-base's 1 from operational failures.
func (e CommandError) ExitCode() int {
	if e.Code == 0 {
		return -1
	}
	return e.Code
}

func (r Runner) Run(ctx context.Context, dir string, args ...string) (string, error) {
	bin := r.Bin
	if bin == "" {
		bin = "git"
	}
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	longClass := ctx.Value(longTransferMarker{}) != nil
	timeoutLabel := timeout
	if deadline, ok := ctx.Deadline(); ok {
		timeoutLabel = time.Until(deadline)
		if timeoutLabel < 0 {
			timeoutLabel = 0
		}
	} else if timeout > 0 && !longClass {
		// A marked transfer-class ctx with no deadline is explicitly
		// unbounded (LongTimeout <= 0); everything else gets the short cap.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
		timeoutLabel = timeout
	}
	args = secureArgs(args)
	//nolint:gosec // Runner constrains git arguments with secureArgs and a sanitized non-interactive environment.
	cmd := exec.CommandContext(ctx, bin, args...)
	// Backstop so a timed-out/cancelled git cannot block Wait forever when a
	// grandchild (ssh, credential helper, git-remote-*) keeps the output pipe
	// open after the direct child is killed.
	cmd.WaitDelay = 10 * time.Second
	if dir != "" {
		cmd.Dir = dir
	}
	env, err := gitEnv()
	if err != nil {
		return "", err
	}
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Stdin = nil
	if err := cmd.Run(); err != nil {
		exitCode := -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" {
			msg = err.Error()
		}
		argText := redactGitText(strings.Join(args, " "))
		msg = redactGitText(msg)
		kind := classifyGitError(msg)
		// Any deadline expiry — the runner's own or a caller-supplied one —
		// is terminal ErrTimeout and never retried; caller CANCELLATION
		// (context.Canceled) still routes through classifyGitError below.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			msg := fmt.Sprintf("timed out after %s", timeoutLabel)
			if longClass {
				// Only transfer-class commands honor the config knob; a hint
				// on a 2m rev-parse/push-metadata timeout would misdirect.
				msg += " (raise materialization.clone_timeout for large repos)"
			}
			return "", CommandError{Kind: ErrTimeout, Args: argText, Message: msg, Code: exitCode}
		}
		return "", CommandError{Kind: kind, Args: argText, Message: msg, Code: exitCode}
	}
	return strings.TrimSpace(stdout.String()), nil
}

// CloneOptions controls git clone behavior (GIT-06).
type CloneOptions struct {
	Partial              bool // --filter=blob:none (blobless clone)
	Submodules           bool // --recurse-submodules so the tree is fully present
	AlsoFilterSubmodules bool // --also-filter-submodules (keep submodules blobless too; only meaningful with Partial)
}

func (r Runner) Clone(ctx context.Context, remote, dest string, partial bool) error {
	return r.CloneWithOptions(ctx, remote, dest, CloneOptions{Partial: partial})
}

// CloneWithOptions runs a git clone with the given options and the GIT-02
// clean-destination retry. When Submodules is set the clone initializes
// submodules so the working tree is structurally complete (GIT-06); with
// Partial + AlsoFilterSubmodules the submodules are blobless too.
func (r Runner) CloneWithOptions(ctx context.Context, remote, dest string, opts CloneOptions) error {
	if err := ValidateRemote(remote); err != nil {
		return err
	}
	args := cloneArgs(remote, dest, opts)
	attempts := r.RetryAttempts
	if attempts <= 0 {
		attempts = 1
	}
	backoff := r.RetryBackoff
	cap := r.RetryCap
	if cap <= 0 {
		cap = 5 * time.Second
	}
	start := time.Now()
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		// GIT-02: a mid-clone network failure (early EOF / RPC failed /
		// connection reset, all classified ErrNetwork) leaves dest partially
		// populated. git does not remove a directory it did not create (dest
		// is a pre-existing os.MkdirTemp dir), so a naive retry of the same
		// argv fails with "destination path already exists and is not empty"
		// and turns a recoverable transient failure into a fatal one. Reset
		// dest to a clean, empty directory before every retry so the clone is
		// idempotent and a transient mid-clone failure is recoverable.
		if attempt > 1 {
			if err := os.RemoveAll(dest); err != nil {
				return fmt.Errorf("clean clone destination for retry: %w", err)
			}
			if err := os.MkdirAll(dest, 0o750); err != nil {
				return fmt.Errorf("recreate clone destination for retry: %w", err)
			}
		}
		// P6-GIT-01: apply the long transfer deadline per attempt, not across
		// the whole retry loop, so a slow failed transfer does not starve a
		// later retry after a genuine transient network error.
		attemptCtx, cancel := r.longTransferContext(ctx)
		_, err := r.Run(attemptCtx, "", args...)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if !errors.Is(err, ErrNetwork) || attempt == attempts {
			return err
		}
		// QUAL-06: stop retrying once the aggregate operation budget is spent.
		if r.MaxElapsed > 0 && time.Since(start) >= r.MaxElapsed {
			return err
		}
		if err := sleepBackoff(ctx, backoff, cap, attempt); err != nil {
			return err
		}
	}
	return lastErr
}

// cloneArgs builds the argv for a git clone with optional blobless partial
// clone (GIT-02) and submodule materialization (GIT-06).
func cloneArgs(remote, dest string, opts CloneOptions) []string {
	args := []string{"clone"}
	if opts.Partial {
		args = append(args, "--filter=blob:none")
		if opts.AlsoFilterSubmodules {
			args = append(args, "--also-filter-submodules")
		}
	}
	if opts.Submodules {
		args = append(args, "--recurse-submodules")
	}
	args = append(args, "--", remote, dest)
	return args
}

func (r Runner) Fetch(ctx context.Context, dir, remote, branch string) error {
	if !safeRemoteName(remote) {
		return fmt.Errorf("invalid git remote name %q", remote)
	}
	args := []string{"fetch", remote}
	if branch != "" {
		if !safeBranchName(branch) {
			return fmt.Errorf("invalid git branch name %q", branch)
		}
		args = append(args, branch)
	}
	args = append(args, "--prune")
	return r.runWithNetworkRetry(ctx, dir, args...)
}

// longTransferMarker tags a context as belonging to the network-transfer
// command class (clone/fetch/push/LFS), so Run can scope the
// materialization.clone_timeout hint to commands that actually honor it and
// skip its short default when the class is explicitly unbounded.
type longTransferMarker struct{}

func (r Runner) longTransferContext(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx = context.WithValue(ctx, longTransferMarker{}, true)
	if r.LongTimeout <= 0 {
		// Explicit no-ceiling: the transfer runs unbounded (Run skips its
		// short default for the marked class) instead of silently falling
		// back to the 2m cap this fix removed.
		return ctx, func() {}
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, r.LongTimeout)
}

// PushBranch pushes branch to remote with -u under the long transfer deadline
// (P6-GIT-01): a large branch push is the same network-transfer class as
// clone/fetch. No retry loop — the wrapper cannot know a failed push is safe
// to repeat, so the caller decides.
func (r Runner) PushBranch(ctx context.Context, dir, remote, branch string) error {
	ctx, cancel := r.longTransferContext(ctx)
	defer cancel()
	_, err := r.Run(ctx, dir, "push", "-u", remote, branch)
	return err
}

// StashCreate runs `git stash create`, producing a commit object WITHOUT
// touching the worktree or index (unlike `git stash push`). Empty stdout
// means there is nothing to stash (a clean working tree) — this is NOT an
// error, ok is simply false.
// UntrackedCount returns how many untracked, non-ignored files the working tree
// holds. It exists because `git stash create` — the primitive the WIP plane
// captures with — does NOT include untracked files and has no `-u` equivalent,
// so an untracked-only tree yields no stash object at all. Without this count
// the plane reported such a tree as "clean" (P9-WIP-02), which is the single
// most misleading thing it could say: a brand-new uncommitted file is exactly
// what "forgot to push" usually means.
func (r Runner) UntrackedCount(ctx context.Context, dir string) (int, error) {
	out, err := r.Run(ctx, dir, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "??") {
			n++
		}
	}
	return n, nil
}

// StashCreate captures the working tree's uncommitted state as a commit object
// without touching the worktree or index. NOTE: `git stash create` does not
// capture UNTRACKED files, and unlike `git stash push` it has no `-u` form —
// capturing them would require mutating the worktree, which this plane exists
// not to do. Callers must therefore consult UntrackedCount before describing a
// tree as clean or a capture as complete (P9-WIP-02).
func (r Runner) StashCreate(ctx context.Context, dir string) (sha string, ok bool, err error) {
	out, err := r.Run(ctx, dir, "stash", "create")
	if err != nil {
		return "", false, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", false, nil
	}
	return out, true, nil
}

// PushRef force-pushes the exact object sha to ref on remote via a raw
// refspec (`git push <remote> +<sha>:<ref>`), distinct from PushBranch's
// tracking branch push. Used by the working-state WIP-ref plane (Layer B,
// spec/07) to push a stash-create commit to
// refs/devstrap/wip/<device_id>/<path_key> without creating or touching any
// local branch. The `+` prefix (a per-refspec force, not a blanket --force)
// is required, not merely a safety margin: ref is this device's OWN
// exclusive namespace segment (safeRefPath's device-id/path-key structure
// guarantees no other device ever writes to it), so a second wip push for
// the same project is expected and common — without it, git refuses every
// non-fast-forward update (a fresh git stash create commit is never a
// descendant of the previous one) with a raw "hint: use git pull" error that
// makes no sense for a ref no one ever pulls into a branch. Runs under the
// same long-transfer deadline class as other network pushes.
func (r Runner) PushRef(ctx context.Context, dir, remote, sha, ref string) error {
	if !safeRemoteName(remote) {
		return fmt.Errorf("invalid git remote name %q", remote)
	}
	if !safeRefPath(ref) {
		return fmt.Errorf("invalid git ref %q", ref)
	}
	// A full hex object id only (CodeRabbit, PR #220): an EMPTY sha would
	// turn the refspec into "+:<ref>" — git's DELETE syntax — silently
	// destroying the remote WIP state if a caller ever ignored StashCreate's
	// ok==false; anything non-hex (option-shaped, a ref name, a revision
	// expression) is equally not what this primitive is documented to push.
	if !isHexObjectID(sha) {
		return fmt.Errorf("invalid git object id %q", sha)
	}
	ctx, cancel := r.longTransferContext(ctx)
	defer cancel()
	_, err := r.Run(ctx, dir, "push", remote, "+"+sha+":"+ref)
	return err
}

// FetchRef fetches ref from remote into the identical local ref path
// (`git fetch <remote> <ref>:<ref>`), used by the working-state WIP-ref plane
// (Layer B) to mirror a peer's refs/devstrap/wip/<device_id>/<path_key> ref
// locally under the exact same name. ref is validated with safeRefPath (the
// same WIP-namespace-scoped check PushRef uses) — this is the one place a
// peer-supplied ref string (read from the device_wip mirror) is ever handed
// to a git subprocess, so validating it here is the caller's actual trust
// boundary, not merely a mirror-storage nicety.
func (r Runner) FetchRef(ctx context.Context, dir, remote, ref string) error {
	if !safeRemoteName(remote) {
		return fmt.Errorf("invalid git remote name %q", remote)
	}
	if !safeRefPath(ref) {
		return fmt.Errorf("invalid git ref %q", ref)
	}
	// The `+` prefix forces the local ref update, mirroring PushRef's own
	// force-push. Empirically confirmed necessary: ref is force-pushed by its
	// owning device on every wip push (each fresh git stash create commit is
	// a sibling of the previous one, never its descendant), so a SECOND local
	// fetch of an already-once-fetched ref is a non-fast-forward update on
	// the fetch side too and would otherwise be rejected outright with a raw
	// "! [rejected] ... (non-fast-forward)" error — exactly the same bug
	// class PushRef had, just on the pull side.
	return r.runWithNetworkRetry(ctx, dir, "fetch", remote, "+"+ref+":"+ref)
}

// DeleteRef deletes ref from remote via a raw refspec push with an empty
// source (`git push <remote> :<ref>`), used by the working-state WIP-ref
// plane (Layer B, spec/07) for `wip drop`.
//
// When expectedSHA is non-empty the delete is a COMPARE-AND-DELETE: the
// explicit-value form `--force-with-lease=<ref>:<sha>` makes the server
// refuse the update unless the ref still points exactly at expectedSHA at
// push time. This is what protects a drop driven by a possibly-stale
// device_wip mirror row — the owning device may have force-pushed a NEWER
// snapshot whose repo.wip.pushed event has not synced here yet, and an
// unconditional delete would destroy that newer recovery data. A lease
// rejection surfaces as ErrNonFastForward ("stale info"); a lease against an
// already-deleted ref is ALSO rejected (absent != expectedSHA), so callers
// that want already-gone idempotency must disambiguate with LsRemoteRef.
//
// With expectedSHA == "" the delete is unconditional; deleting an
// already-nonexistent ref is then NOT an error — git prints a stderr warning
// ("deleting a non-existent ref") but still exits 0 — empirically confirmed;
// do not treat that warning as a failure.
func (r Runner) DeleteRef(ctx context.Context, dir, remote, ref, expectedSHA string) error {
	if !safeRemoteName(remote) {
		return fmt.Errorf("invalid git remote name %q", remote)
	}
	if !safeRefPath(ref) {
		return fmt.Errorf("invalid git ref %q", ref)
	}
	args := []string{"push"}
	if expectedSHA != "" {
		args = append(args, "--force-with-lease="+ref+":"+expectedSHA)
	}
	args = append(args, remote, ":"+ref)
	_, err := r.Run(ctx, dir, args...)
	return err
}

// LsRemoteRef returns the sha the remote currently advertises for exactly
// ref, or ErrBranchNotFound when the remote has no such ref. Used by `wip
// drop` to tell a lease-rejected delete's two causes apart: the ref is
// already gone (idempotent success) vs. the ref moved to a sha the local
// mirror does not know about (refuse — newer recovery data exists).
func (r Runner) LsRemoteRef(ctx context.Context, dir, remote, ref string) (string, error) {
	if !safeRemoteName(remote) {
		return "", fmt.Errorf("invalid git remote name %q", remote)
	}
	if !safeRefPath(ref) {
		return "", fmt.Errorf("invalid git ref %q", ref)
	}
	ctx, cancel := r.longTransferContext(ctx)
	defer cancel()
	out, err := r.Run(ctx, dir, "ls-remote", remote, ref)
	if err != nil {
		return "", err
	}
	// Output shape: "<sha>\t<ref>\n" per advertised ref; empty when absent.
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == ref {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrBranchNotFound, ref)
}

type RemoteRef struct {
	Ref string
	SHA string
}

const wipRefGlob = "refs/devstrap/wip/*"

// LsRemoteWipRefs enumerates the remote WIP namespace without writing any
// local refs. That structural property keeps worktree base resolution isolated
// from recovery refs.
func (r Runner) LsRemoteWipRefs(ctx context.Context, dir, remote string) (refs []RemoteRef, skipped int, err error) {
	if !safeRemoteName(remote) {
		return nil, 0, fmt.Errorf("invalid git remote name %q", remote)
	}
	ctx, cancel := r.longTransferContext(ctx)
	defer cancel()
	out, err := r.Run(ctx, dir, "ls-remote", remote, wipRefGlob)
	if err != nil {
		return nil, 0, err
	}
	refs, skipped = parseLsRemoteWipRefs(out)
	return refs, skipped, nil
}

func parseLsRemoteWipRefs(out string) ([]RemoteRef, int) {
	var refs []RemoteRef
	skipped := 0
	lines := strings.Split(out, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1] // ordinary command-output terminator
	}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			skipped++
			continue
		}
		if len(fields) != 2 || !isHexObjectID(fields[0]) || !safeRefPath(fields[1]) ||
			!strings.HasPrefix(fields[1], "refs/devstrap/wip/") {
			skipped++
			continue
		}
		refs = append(refs, RemoteRef{Ref: fields[1], SHA: fields[0]})
	}
	return refs, skipped
}

// MaintenanceRun runs a one-time `git maintenance run --auto` (commit-graph +
// prefetch) so common history ops (blame, log -p) do not trigger per-object
// lazy fetches on a blobless clone (GIT-06). It is best-effort: older git or a
// missing promisor makes this a no-op or error, and the caller should not fail
// materialization on it.
func (r Runner) MaintenanceRun(ctx context.Context, dir string) error {
	_, err := r.Run(ctx, dir, "maintenance", "run", "--auto")
	return err
}

func (r Runner) RemoteURL(ctx context.Context, dir string) (string, error) {
	out, err := r.Run(ctx, dir, "remote", "get-url", "origin")
	if errors.Is(err, ErrRemoteMissing) {
		return "", fmt.Errorf("%w: origin", err)
	}
	return out, err
}

func (r Runner) runWithNetworkRetry(ctx context.Context, dir string, args ...string) error {
	attempts := r.RetryAttempts
	if attempts <= 0 {
		attempts = 1
	}
	backoff := r.RetryBackoff
	cap := r.RetryCap
	if cap <= 0 {
		cap = 5 * time.Second
	}
	start := time.Now()
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		// P6-GIT-01: apply the long transfer deadline per attempt, not across
		// the whole retry loop, so a slow failed transfer does not starve a
		// later retry after a genuine transient network error.
		attemptCtx, cancel := r.longTransferContext(ctx)
		_, err := r.Run(attemptCtx, dir, args...)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if !errors.Is(err, ErrNetwork) || attempt == attempts {
			return err
		}
		// QUAL-06: stop retrying once the aggregate operation budget is spent.
		if r.MaxElapsed > 0 && time.Since(start) >= r.MaxElapsed {
			return err
		}
		if err := sleepBackoff(ctx, backoff, cap, attempt); err != nil {
			return err
		}
	}
	return lastErr
}

// sleepBackoff waits for a full-jitter capped-exponential backoff delay or
// until ctx is cancelled (QUAL-06). A non-positive base returns immediately so
// the next attempt runs without delay. Without jitter, parallel materialize
// workers retry in lockstep at identical boundaries (a synchronized thundering
// herd that amplifies load on a struggling forge); full jitter spreads retries
// uniformly across [1, min(cap, base*2^(attempt-1))], the AWS-recommended scheme.
func sleepBackoff(ctx context.Context, base, cap time.Duration, attempt int) error {
	if base <= 0 {
		return nil
	}
	timer := time.NewTimer(jitterDelay(base, cap, attempt, rand.Int63n))
	select {
	case <-ctx.Done():
		timer.Stop()
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// jitterDelay computes a full-jitter capped-exponential backoff delay for the
// given attempt. The result is uniform in [1, min(cap, base*2^(attempt-1))].
// It takes a randFn so it is deterministic under a seeded RNG in tests.
func jitterDelay(base, cap time.Duration, attempt int, randFn func(n int64) int64) time.Duration {
	if base <= 0 {
		return 0
	}
	maxN := int64(cap)
	if exp := int64(base) * (int64(1) << uint(attempt-1)); exp < maxN {
		maxN = exp
	}
	if maxN < 1 {
		maxN = 1
	}
	return time.Duration(randFn(maxN) + 1)
}

// DefaultBranchSource records how ResolveDefaultBranch determined the branch,
// from most to least authoritative.
type DefaultBranchSource string

const (
	// DefaultBranchRemote means the value came from the remote (origin/HEAD or
	// a set-head --auto query).
	DefaultBranchRemote DefaultBranchSource = "remote"
	// DefaultBranchStored means origin/HEAD was unavailable and a previously
	// stored fallback branch was verified to exist on the remote.
	DefaultBranchStored DefaultBranchSource = "stored"
)

// DefaultBranch resolves the remote default branch, returning only the branch
// name. Prefer ResolveDefaultBranch when the caller wants to know how
// authoritative the answer is.
func (r Runner) DefaultBranch(ctx context.Context, dir, fallback string) (string, error) {
	branch, _, err := r.ResolveDefaultBranch(ctx, dir, fallback)
	return branch, err
}

// ResolveDefaultBranch resolves the remote default branch in layers, never
// silently falling back to a hardcoded "main": (1) read refs/remotes/origin/HEAD;
// (2) on failure, repair it with `remote set-head origin --auto` (which queries
// the remote) and retry; (3) verify the caller's stored fallback exists on the
// remote. It returns the branch and the source so callers can warn when the
// answer is not authoritative.
func (r Runner) ResolveDefaultBranch(ctx context.Context, dir, fallback string) (string, DefaultBranchSource, error) {
	if branch, ok := r.symbolicOriginHead(ctx, dir); ok {
		if !safeBranchName(branch) {
			return "", "", fmt.Errorf("invalid origin HEAD branch %q", branch)
		}
		return branch, DefaultBranchRemote, nil
	}
	// origin/HEAD is missing or stale (common after single-branch/mirror clones);
	// ask the remote to set it, then retry.
	_, _ = r.Run(ctx, dir, "remote", "set-head", "origin", "--auto")
	if branch, ok := r.symbolicOriginHead(ctx, dir); ok {
		if !safeBranchName(branch) {
			return "", "", fmt.Errorf("invalid origin HEAD branch %q", branch)
		}
		return branch, DefaultBranchRemote, nil
	}
	if fallback != "" {
		if !safeBranchName(fallback) {
			return "", "", fmt.Errorf("invalid fallback branch %q", fallback)
		}
		if _, err := r.RevParse(ctx, dir, "origin/"+fallback); err == nil {
			return fallback, DefaultBranchStored, nil
		}
		return "", "", fmt.Errorf("origin default branch unavailable and fallback %q was not found", fallback)
	}
	return "", "", fmt.Errorf("origin default branch unavailable")
}

// LocalDefaultBranch resolves the default branch using only local refs — it
// never touches the network (no set-head/ls-remote/fetch). Scan-time onboarding
// must stay offline (P6-XP-05); authoritative set-head --auto repair is deferred
// to hydrate/worktree materialization, which calls ResolveDefaultBranch at use
// time. It returns the branch and how authoritative the answer is.
func (r Runner) LocalDefaultBranch(ctx context.Context, dir, fallback string) (string, DefaultBranchSource, error) {
	if branch, ok := r.symbolicOriginHead(ctx, dir); ok {
		if !safeBranchName(branch) {
			return "", "", fmt.Errorf("invalid origin HEAD branch %q", branch)
		}
		return branch, DefaultBranchRemote, nil
	}
	if fallback != "" {
		if !safeBranchName(fallback) {
			return "", "", fmt.Errorf("invalid fallback branch %q", fallback)
		}
		// origin/<fallback> is a local remote-tracking ref check (rev-parse),
		// not a network call.
		if _, err := r.RevParse(ctx, dir, "origin/"+fallback); err == nil {
			return fallback, DefaultBranchStored, nil
		}
	}
	return "", "", fmt.Errorf("origin default branch unavailable offline")
}

func (r Runner) symbolicOriginHead(ctx context.Context, dir string) (string, bool) {
	// Read the FULL symbolic-ref target, not --short: --short generically
	// strips any leading well-known-looking segment for ANY ref shape, not
	// only a legitimate refs/remotes/origin/<branch> target — a HEAD symref
	// repointed at a non-remote-tracking ref (e.g. refs/devstrap/wip/<device>/
	// <path_key>, working-state validation plane Layer B; empirically
	// confirmed: `git symbolic-ref --short` on such a target returns
	// "devstrap/wip/..." verbatim, which the old origin/-prefix trim below
	// would then pass straight through unchanged) would otherwise validate as
	// if it were a legitimate branch name. `git remote set-head origin --auto`
	// writes this ref from whatever the remote reports, so a hostile or
	// misconfigured remote can reach this path, not only local tampering.
	out, err := r.Run(ctx, dir, "symbolic-ref", "refs/remotes/origin/HEAD")
	if err != nil {
		return "", false
	}
	const originPrefix = "refs/remotes/origin/"
	target := strings.TrimSpace(out)
	if !strings.HasPrefix(target, originPrefix) {
		return "", false
	}
	branch := strings.TrimPrefix(target, originPrefix)
	if branch == "" {
		return "", false
	}
	return branch, true
}

// RemoteDefaultBranch queries the remote authoritatively with
// `git ls-remote --symref <remote> HEAD`, returning the branch HEAD points at.
// This works even when no local refs/remotes/origin/HEAD exists. It is a
// network operation.
func (r Runner) RemoteDefaultBranch(ctx context.Context, dir, remote string) (string, error) {
	if !safeRemoteName(remote) {
		return "", fmt.Errorf("invalid git remote name %q", remote)
	}
	out, err := r.Run(ctx, dir, "ls-remote", "--symref", remote, "HEAD")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ref:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "ref:"))
		if len(fields) < 2 || fields[1] != "HEAD" {
			continue
		}
		// The symref target MUST be under refs/heads/ — TrimPrefix alone is a
		// no-op passthrough for anything else (refs/tags/*, refs/devstrap/wip/*,
		// ...), and safeBranchName tolerates slashes (by design, for real
		// branch names like "feature/foo"), so a HEAD symref repointed at a
		// non-branch ref would otherwise validate and be returned as if it
		// were a legitimate default branch name. A hostile or misconfigured
		// remote repointing HEAD at refs/devstrap/wip/<device>/<path_key>
		// (working-state validation plane Layer B, spec/10's agent-isolation
		// invariant) must fail here, not merely fail later by accident of
		// whatever local fetch refspec happens to be configured.
		const headsPrefix = "refs/heads/"
		if !strings.HasPrefix(fields[0], headsPrefix) {
			return "", fmt.Errorf("remote %q HEAD points at non-branch ref %q", remote, fields[0])
		}
		branch := strings.TrimPrefix(fields[0], headsPrefix)
		if branch == "" || !safeBranchName(branch) {
			return "", fmt.Errorf("invalid remote HEAD ref %q", fields[0])
		}
		return branch, nil
	}
	return "", fmt.Errorf("remote %q did not report a symbolic HEAD", remote)
}

func (r Runner) RevParse(ctx context.Context, dir, ref string) (string, error) {
	return r.Run(ctx, dir, "rev-parse", ref)
}

// MergeBase resolves the merge base of a and b (`git merge-base a b`). git's
// own exit-1/empty-output convention for "no common ancestor" (two refs with
// unrelated histories, e.g. an orphan gh-pages branch) is NOT an operational
// failure — it is classified as the distinct sentinel ErrNoMergeBase so
// callers can offer an explicit remedy (an alternate --base-ref) instead of
// failing bare. Any other non-zero exit (invalid ref, git too old) is
// returned as an ordinary error.
func (r Runner) MergeBase(ctx context.Context, dir, a, b string) (string, error) {
	out, err := r.Run(ctx, dir, "merge-base", a, b)
	if err != nil {
		var cmdErr CommandError
		if errors.As(err, &cmdErr) && cmdErr.ExitCode() == 1 {
			return "", fmt.Errorf("%w: %s and %s", ErrNoMergeBase, a, b)
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// RemoteTrackingContains reports whether sha is reachable from ANY
// remote-tracking ref in dir (`git branch -r --contains <sha>`).
//
// It exists for the `--pinned` manifest export, where the distinction is the
// whole point: a SHA that lives only in the local checkout is worthless in the
// disaster the pin is written for, because after total local loss it exists
// nowhere. An unpushed commit and a topic-branch HEAD both produce a pin that
// `vcs import` cannot check out.
//
// No network: this reads refs already fetched into refs/remotes. A stale
// remote-tracking ref can therefore answer "no" for a commit that IS on the
// remote — the safe direction, since the caller degrades to omitting the
// version rather than recording one it cannot vouch for.
func (r Runner) RemoteTrackingContains(ctx context.Context, dir, sha string) (bool, error) {
	out, err := r.Run(ctx, dir, "branch", "-r", "--contains", sha, "--format=%(refname)")
	if err != nil {
		var cmdErr CommandError
		// git exits non-zero when the object is unknown to this repository,
		// which is itself a definitive "not reachable" rather than a failure.
		if errors.As(err, &cmdErr) && cmdErr.ExitCode() != 0 {
			return false, nil
		}
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// IsShallow reports whether dir is a shallow clone (`git rev-parse
// --is-shallow-repository`). A shallow history can make MergeBase return a
// plausible-but-wrong answer at the shallow boundary rather than the true
// common ancestor, so callers that record a merge-base as provenance (e.g.
// `worktree adopt`) must gate on this before trusting the result.
func (r Runner) IsShallow(ctx context.Context, dir string) (bool, error) {
	out, err := r.Run(ctx, dir, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "true", nil
}

// WorktreeIdentity resolves what dir actually is: the main checkout, a
// genuine linked worktree, or neither (not a git worktree at all). It is the
// single definition behind both WorktreeSandboxWriteDirs' sandbox-grant
// resolution and `worktree adopt`'s registration of an externally-created
// worktree — extracted so there is exactly one place that decides "linked
// worktree or not" (previously duplicated logic living only inside
// WorktreeSandboxWriteDirs).
type WorktreeIdentity struct {
	CommonDir    string // resolved git-common-dir
	GitDir       string // resolved git-dir (per-worktree admin dir for a linked worktree)
	IsLinked     bool   // true only for a genuine linked worktree
	MainCheckout string // working tree of the main checkout
	Branch       string // "" when HEAD is detached
	HeadSHA      string // "" when HEAD is unborn
}

func (r Runner) WorktreeIdentity(ctx context.Context, dir string) (WorktreeIdentity, error) {
	common, err := r.Run(ctx, dir, "rev-parse", "--git-common-dir")
	if err != nil {
		return WorktreeIdentity{}, err
	}
	gitDir, err := r.Run(ctx, dir, "rev-parse", "--git-dir")
	if err != nil {
		return WorktreeIdentity{}, err
	}
	abs := func(p string) string {
		p = strings.TrimSpace(p)
		if p == "" {
			return ""
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		if resolved, rerr := filepath.EvalSymlinks(p); rerr == nil {
			return resolved
		}
		return filepath.Clean(p)
	}
	commonAbs := abs(common)
	gitDirAbs := abs(gitDir)
	if commonAbs == "" || gitDirAbs == "" {
		return WorktreeIdentity{}, fmt.Errorf("could not resolve git dir for %q", dir)
	}
	// A genuine linked worktree's --git-dir sits under <common>/worktrees/; the
	// main checkout has gitDirAbs == commonAbs. A --git-dir that resolves
	// elsewhere under the common dir (a malformed gitfile/commondir could point
	// at <common>/hooks or a sibling) is refused rather than treated as linked.
	isLinked := false
	if gitDirAbs != commonAbs {
		worktreesRoot := filepath.Join(commonAbs, "worktrees")
		if rel, rerr := filepath.Rel(worktreesRoot, gitDirAbs); rerr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			isLinked = true
		}
	}
	// commonAbs normally ends in "/.git", so its main checkout is its parent
	// directory. A bare repo, or a checkout made with `git clone/init
	// --separate-git-dir`, does not — and guessing there would fabricate a wrong
	// path. Such a case yields MainCheckout == "" rather than an ERROR, which
	// matters: WorktreeSandboxWriteDirs must keep granting git-storage writes for
	// these layouts (it only needs CommonDir and IsLinked), and turning this into
	// an error would silently deny every git write for a --separate-git-dir
	// project, breaking the agent's own `git commit` under the sandbox. Callers
	// that genuinely need the main checkout — `worktree adopt` — check for "" and
	// refuse themselves.
	mainCheckout := ""
	if filepath.Base(commonAbs) == ".git" {
		mainCheckout = filepath.Dir(commonAbs)
	}
	branch := ""
	if out, berr := r.Run(ctx, dir, "symbolic-ref", "--quiet", "--short", "HEAD"); berr == nil {
		branch = strings.TrimSpace(out)
	}
	headSHA := ""
	if out, herr := r.Run(ctx, dir, "rev-parse", "--verify", "HEAD"); herr == nil {
		headSHA = strings.TrimSpace(out)
	}
	return WorktreeIdentity{
		CommonDir:    commonAbs,
		GitDir:       gitDirAbs,
		IsLinked:     isLinked,
		MainCheckout: mainCheckout,
		Branch:       branch,
		HeadSHA:      headSHA,
	}, nil
}

// IsSquashMerged reports whether branch's content is already contained in the
// CURRENT baseRef tree — the content-equivalence test behind `worktree cleanup
// --merged`'s squash/rebase detection (P4-GIT-04). It simulates the merge
// (`git merge-tree --write-tree <baseRef> <branch>`, git >= 2.38): when the
// resulting tree is identical to baseRef's own tree, merging the branch would
// contribute nothing — every change the branch carries is already present in
// base, which is exactly the effect of a squash- or rebase-merge. Comparing
// against the CURRENT tree (rather than patch-id history) means a change that
// was merged and then REVERTED on base correctly reads as NOT merged
// (dual-review finding: historical patch-id equivalence would have deleted
// genuinely-unmerged work).
//
// Conservative by construction: a conflicting simulated merge (content
// diverged), an invalid ref, or an older git without --write-tree all report
// false — doubt is never grounds to reap.
//
// Documented accepted limitation (inherent to ANY content-equivalence test):
// a branch whose net change ALSO landed via an unrelated identical commit is
// indistinguishable from a squash-merge and reads as merged; the reap
// breadcrumb (the printed branch tip SHA) is the recovery path. Pinned by
// TestIsSquashMergedMatchesCoincidentallyIdenticalDiff.
func (r Runner) IsSquashMerged(ctx context.Context, dir, branch, baseRef string) (bool, error) {
	if !safeBranchName(branch) {
		return false, fmt.Errorf("invalid git branch name %q", branch)
	}
	if !safeBranchName(baseRef) {
		return false, fmt.Errorf("invalid git base ref %q", baseRef)
	}
	merged, err := r.Run(ctx, dir, "merge-tree", "--write-tree", baseRef, branch)
	if err != nil {
		// Exit 1 is a conflicting simulated merge — the canonical not-merged
		// answer. Any other failure (old git, bad ref) is equally
		// conservative: never reap on doubt.
		return false, nil
	}
	baseTree, err := r.Run(ctx, dir, "rev-parse", baseRef+"^{tree}")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(merged) == strings.TrimSpace(baseTree), nil
}

// WorktreeSandboxWriteDirs returns the absolute git storage paths a linked
// worktree must be able to WRITE for `git add`/`git commit` to succeed under an
// OS sandbox: the shared object store, refs, and reflogs in the git-common-dir,
// plus the per-worktree admin dir (index/HEAD/COMMIT_EDITMSG/logs). It
// deliberately EXCLUDES the common dir itself (and thus hooks/ and config) —
// granting those would let a sandboxed agent plant a hook or config that
// executes UNSANDBOXED on a later git operation (P7-SANDBOX-01). Paths are
// symlink-resolved. A nil slice with no error is returned when dir is not inside
// a git worktree, so callers can grant nothing without special-casing.
func (r Runner) WorktreeSandboxWriteDirs(ctx context.Context, dir string) ([]string, error) {
	identity, err := r.WorktreeIdentity(ctx, dir)
	if err != nil {
		// Translate any resolution failure (not a git worktree, bare repo,
		// relocated $GIT_COMMON_DIR, ...) into WorktreeSandboxWriteDirs' own
		// long-standing nil,nil contract: callers grant nothing rather than
		// special-casing an error they cannot act on.
		return nil, nil
	}
	// Resolve each storage subpath too: <common>/objects can itself be a
	// symlink (git alternates / a relocated object store), and Seatbelt matches
	// the kernel-real path — so grant the resolved target, not the alias.
	// EvalSymlinks fails on a not-yet-created path (e.g. no reflog); fall back
	// to the clean join, which git creates under the already-resolved commonAbs.
	evalJoin := func(elem string) string {
		p := filepath.Join(identity.CommonDir, elem)
		if resolved, rerr := filepath.EvalSymlinks(p); rerr == nil {
			return resolved
		}
		return p
	}
	dirs := []string{
		evalJoin("objects"),
		evalJoin("refs"),
		evalJoin("logs"),
	}
	// Add the per-worktree admin dir only for a genuine LINKED worktree. The
	// main checkout (objects/refs/logs already cover its writes) never gets
	// the whole common dir — hooks/ and config live there.
	if identity.IsLinked {
		dirs = append(dirs, identity.GitDir)
	}
	return dirs, nil
}

// WorktreeSandboxDenyFiles returns the absolute paths, INSIDE the per-worktree
// admin dir WorktreeSandboxWriteDirs grants writable, that must stay read-only:
// `commondir` and `gitdir` (the pointers to the shared .git) and
// `config.worktree`. Rewriting `commondir` relocates git's whole configuration
// into attacker-controlled space, so the next UNSANDBOXED git command in that
// worktree executes whatever hook/fsmonitor it names — P8-SEC-02, the same
// escape class P7-SANDBOX-01 closed for the common dir itself. Git writes these
// files only at `git worktree add` time and only reads them thereafter, so
// denying writes costs nothing. Mirrors WorktreeSandboxWriteDirs' contract: a
// nil slice with no error means "nothing to deny" for a main checkout or a dir
// that is not a git worktree at all, so callers can grant the write set and
// deny this set without special-casing either.
func (r Runner) WorktreeSandboxDenyFiles(ctx context.Context, dir string) ([]string, error) {
	identity, err := r.WorktreeIdentity(ctx, dir)
	if err != nil {
		return nil, nil
	}
	if !identity.IsLinked {
		return nil, nil
	}
	// identity.GitDir is already symlink-resolved by WorktreeIdentity, matching
	// WorktreeSandboxWriteDirs' resolution of the same directory.
	return []string{
		filepath.Join(identity.GitDir, "commondir"),
		filepath.Join(identity.GitDir, "gitdir"),
		filepath.Join(identity.GitDir, "config.worktree"),
	}, nil
}

func (r Runner) WorktreeAdd(ctx context.Context, dir, path, branch, base string) error {
	if !safeBranchName(branch) {
		return fmt.Errorf("invalid git branch name %q", branch)
	}
	_, err := r.Run(ctx, dir, "worktree", "add", "-b", branch, "--", path, base)
	return err
}

func (r Runner) WorktreeRemove(ctx context.Context, dir, path string, force bool) error {
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, "--", path)
	_, err := r.Run(ctx, dir, args...)
	return err
}

func (r Runner) WorktreePrune(ctx context.Context, dir string) error {
	_, err := r.Run(ctx, dir, "worktree", "prune")
	return err
}

func (r Runner) LFSPull(ctx context.Context, dir string) error {
	attemptCtx, cancel := r.longTransferContext(ctx)
	defer cancel()
	_, err := r.Run(attemptCtx, dir, "lfs", "pull")
	return err
}

// LFSInstallLocal installs the LFS smudge/clean filters into the repo's own
// .git/config. It is required on the materialize path because gitEnv sets
// GIT_CONFIG_GLOBAL=/dev/null, hiding any global `git lfs install` (P6-GIT-04).
// This is a local operation (no network); it uses the default Timeout.
func (r Runner) LFSInstallLocal(ctx context.Context, dir string) error {
	_, err := r.Run(ctx, dir, "lfs", "install", "--local")
	return err
}

func UsesLFS(ctx context.Context, dir string) (bool, error) {
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() != ".gitattributes" {
			return nil
		}
		//nolint:gosec // WalkDir supplies .gitattributes paths below the inspected repository root.
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if attributesUseLFS(string(raw)) {
			return errUsesLFS
		}
		return nil
	})
	if errors.Is(err, errUsesLFS) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("scan git lfs attributes: %w", err)
	}
	return false, nil
}

var errUsesLFS = errors.New("git lfs attributes found")

func attributesUseLFS(raw string) bool {
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		for _, field := range strings.Fields(line) {
			if field == "filter=lfs" {
				return true
			}
		}
	}
	return false
}

type BaseDrift struct {
	CurrentSHA string `json:"current_sha"`
	Behind     int    `json:"behind"`
	Fresh      bool   `json:"fresh"`
}

func (r Runner) BaseDrift(ctx context.Context, dir, baseRef, recordedSHA string) (BaseDrift, error) {
	remote, branch, ok := strings.Cut(baseRef, "/")
	if !ok || remote == "" || branch == "" {
		return BaseDrift{}, fmt.Errorf("base ref must be remote/branch, got %q", baseRef)
	}
	if err := r.Fetch(ctx, dir, remote, branch); err != nil {
		return BaseDrift{}, err
	}
	current, err := r.RevParse(ctx, dir, baseRef)
	if err != nil {
		return BaseDrift{}, err
	}
	if current == recordedSHA {
		return BaseDrift{CurrentSHA: current, Fresh: true}, nil
	}
	out, err := r.Run(ctx, dir, "rev-list", "--count", recordedSHA+".."+current)
	if err != nil {
		return BaseDrift{}, err
	}
	behind, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return BaseDrift{}, fmt.Errorf("parse base drift count %q: %w", out, err)
	}
	return BaseDrift{CurrentSHA: current, Behind: behind, Fresh: behind == 0}, nil
}

type DirtyState string

const (
	DirtyUnknown    DirtyState = "unknown"
	DirtyClean      DirtyState = "clean"
	DirtyDirty      DirtyState = "dirty"
	DirtyAhead      DirtyState = "ahead"
	DirtyBehind     DirtyState = "behind"
	DirtyDiverged   DirtyState = "diverged"
	DirtyConflicted DirtyState = "conflicted"
)

func (r Runner) DirtyState(ctx context.Context, dir string) (DirtyState, error) {
	out, err := r.Run(ctx, dir, "status", "--porcelain=v2", "--branch")
	if err != nil {
		return DirtyUnknown, err
	}
	hasChange := false
	ahead := 0
	behind := 0
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "u ") {
			return DirtyConflicted, nil
		}
		if strings.HasPrefix(line, "1 ") || strings.HasPrefix(line, "2 ") || strings.HasPrefix(line, "? ") || strings.HasPrefix(line, "! ") {
			hasChange = true
			continue
		}
		if strings.HasPrefix(line, "# branch.ab ") {
			_, _ = fmt.Sscanf(strings.TrimPrefix(line, "# branch.ab "), "+%d -%d", &ahead, &behind)
		}
	}
	switch {
	case hasChange:
		return DirtyDirty, nil
	case ahead > 0 && behind > 0:
		return DirtyDiverged, nil
	case ahead > 0:
		return DirtyAhead, nil
	case behind > 0:
		return DirtyBehind, nil
	default:
		return DirtyClean, nil
	}
}

func IsRepo(path string) bool {
	_, err := filepath.Abs(filepath.Join(path, ".git"))
	if err != nil {
		return false
	}
	return dirExists(filepath.Join(path, ".git")) || fileExists(filepath.Join(path, ".git"))
}

func CanonicalRemoteKey(remote string) (string, error) {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return "", errors.New("remote URL must not be empty")
	}
	if err := ValidateRemote(remote); err != nil {
		return "", err
	}
	if strings.HasPrefix(remote, "git@") || scpLike.MatchString(remote) {
		host, repoPath, ok := splitSCPLikeRemote(remote)
		if !ok {
			return "", fmt.Errorf("invalid scp-like remote %q", remote)
		}
		return normalizeHostPath(host, repoPath), nil
	}
	if strings.HasPrefix(remote, "/") {
		return "file/" + strings.Trim(strings.TrimSuffix(filepath.ToSlash(filepath.Clean(remote)), ".git"), "/"), nil
	}
	u, err := url.Parse(remote)
	if err != nil || u.Host == "" {
		if err == nil && u.Scheme == "file" && u.Path != "" {
			return "file/" + strings.Trim(strings.TrimSuffix(filepath.ToSlash(filepath.Clean(u.Path)), ".git"), "/"), nil
		}
		return "", fmt.Errorf("invalid remote URL %q", remote)
	}
	if u.Scheme == "file" {
		return "file/" + strings.Trim(strings.TrimSuffix(filepath.ToSlash(filepath.Clean(u.Path)), ".git"), "/"), nil
	}
	path := strings.TrimPrefix(u.Path, "/")
	return normalizeHostPath(u.Hostname(), path), nil
}

var scpLike = regexp.MustCompile(`^[^@:/]+@[^:/]+(?::[0-9]+)?:.+`)
var urlCredentials = regexp.MustCompile(`(?i)(https?://)[^/@\s]+@`)

func splitSCPLikeRemote(remote string) (string, string, bool) {
	userHost, repoPath, ok := strings.Cut(remote, ":")
	if !ok || repoPath == "" {
		return "", "", false
	}
	hostPart := userHost
	if before, portAndPath, ok := strings.Cut(repoPath, ":"); ok {
		if _, err := strconv.Atoi(before); err == nil {
			hostPart = userHost + ":" + before
			repoPath = portAndPath
		}
	}
	host := hostPart
	if at := strings.LastIndex(host, "@"); at >= 0 {
		host = host[at+1:]
	}
	if before, after, ok := strings.Cut(host, ":"); ok {
		if _, err := strconv.Atoi(after); err == nil {
			host = before
		}
	}
	return host, repoPath, host != "" && repoPath != ""
}

func normalizeHostPath(host, path string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.TrimSuffix(host, ":22")
	// FORGE-03: Azure DevOps uses divergent SSH/HTTPS shapes that produce
	// different canonical keys. Unify both forms to dev.azure.com/org/proj/repo.
	if host == "ssh.dev.azure.com" {
		host = "dev.azure.com"
		path = strings.TrimPrefix(path, "v3/")
	}
	path = strings.Trim(strings.TrimSpace(path), "/")
	path = strings.TrimSuffix(path, ".git")
	path = strings.Replace(path, "/_git/", "/", 1)
	parts := strings.Split(path, "/")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return host + "/" + strings.Join(parts, "/")
}

func ValidateRemote(remote string) error {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return errors.New("remote URL must not be empty")
	}
	if strings.HasPrefix(remote, "-") {
		return fmt.Errorf("remote URL must not start with '-'")
	}
	if strings.HasPrefix(remote, "git@") || scpLike.MatchString(remote) {
		if _, _, ok := splitSCPLikeRemote(remote); !ok {
			return fmt.Errorf("invalid scp-like remote %q", remote)
		}
		return nil
	}
	if strings.HasPrefix(remote, "/") {
		return nil
	}
	u, err := url.Parse(remote)
	if err != nil {
		return fmt.Errorf("invalid remote URL %q: %w", remote, err)
	}
	switch u.Scheme {
	case "https", "ssh", "git", "file":
		if u.Scheme != "file" && u.Host == "" {
			return fmt.Errorf("remote URL %q must include a host", remote)
		}
		return nil
	default:
		return fmt.Errorf("unsupported git remote scheme %q", u.Scheme)
	}
}

func secureArgs(args []string) []string {
	secure := []string{
		"-c", "protocol.allow=never",
		"-c", "protocol.https.allow=always",
		"-c", "protocol.ssh.allow=always",
		"-c", "protocol.git.allow=always",
		"-c", "protocol.file.allow=always",
		"-c", "protocol.ext.allow=never",
		"-c", "core.sshCommand=ssh -oBatchMode=yes",
		// Keep Git's automatic housekeeping ATTACHED so it finishes before this
		// call returns instead of detaching into a background writer that
		// outlives it — the #174/#176 flake, where gc.log / tmp_pack_* / *.lock
		// appeared in a directory the caller had already finished with.
		//
		// Two keys are deliberately absent, and both are traps:
		//   - gc.auto=0 would make internal/hub's deliberate `git gc --auto`
		//     (gitGCAuto) a no-op, silently retiring the P7-HUB-03 carrier
		//     growth control while every test stayed green.
		//   - maintenance.auto=false would stop EVERY managed clone from running
		//     auto-gc at all, since modern Git triggers post-command
		//     housekeeping only through it — trading an unbounded background
		//     writer for an unbounded repository.
		// TestSecureArgsDisablesDetachedAutoMaintenance asserts both are absent.
		//
		// These are PREPENDED, so a caller's own -c wins under Git's last-wins
		// rule. That is intentional and relied upon — gitGCAuto passes its own
		// gc.autoDetach=false beside the call whose lock-hold guarantee needs
		// it — so this is a default applied to every call, not an enforcement
		// against callers. A cancelled call can also still outlive its
		// maintenance grandchild; see spec/15's stated residual.
		"-c", "gc.autoDetach=false",
		"-c", "maintenance.autoDetach=false",
	}
	return append(secure, args...)
}

func gitEnv() ([]string, error) {
	return childenv.FromOS(childenv.BasicAllowlist(), map[string]string{
		"GIT_ASKPASS":            "",
		"GIT_CONFIG_GLOBAL":      "/dev/null",
		"GIT_CONFIG_NOSYSTEM":    "1",
		"GIT_PROTOCOL_FROM_USER": "0",
		"GIT_TERMINAL_PROMPT":    "0",
		"SSH_ASKPASS":            "",
	})
}

func safeRemoteName(remote string) bool {
	if remote == "" || strings.HasPrefix(remote, "-") || strings.ContainsAny(remote, " \t\n\r") {
		return false
	}
	return regexp.MustCompile(`^[A-Za-z0-9._-]+$`).MatchString(remote)
}

// SafeBranchName reports whether branch is a plain, option-injection-free git
// branch name (the git-carrier hub validates its configured branch with it).
func SafeBranchName(branch string) bool {
	return safeBranchName(branch)
}

func safeBranchName(branch string) bool {
	if branch == "" || strings.HasPrefix(branch, "-") || strings.Contains(branch, "..") {
		return false
	}
	if strings.ContainsAny(branch, " \t\n\r~^:?*[\\") || strings.HasSuffix(branch, ".") || strings.HasSuffix(branch, "/") {
		return false
	}
	for _, part := range strings.Split(branch, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

// safeRefPath reports whether ref is a safe, option-injection-free full ref
// under the devstrap WIP-ref namespace: refs/devstrap/wip/<device-id
// segment>/<path_key segment(s)>. PushRef is documented as the primitive
// backing that one namespace (Layer B, spec/07) and pushes to a REMOTE, so
// this is intentionally a NARROWER check than safeBranchName's generic
// multi-segment support — safeBranchName already tolerates internal slashes
// and would happily accept a "refs/devstrap/wip/..." string (it has no
// notion of a required prefix), which is fine for its actual call sites
// (real branch names) but would let a mistaken or miscomputed ref land
// anywhere PushBranch's targets can (e.g. refs/heads/main) if reused
// unchanged here. Loosening safeBranchName itself is deliberately avoided —
// it is still used to validate real branch names elsewhere, and broadening
// its character class would weaken those call sites too. The bulk of the
// character-class work (no "..", no whitespace/control/~^:?*[\ chars, no
// leading "." or trailing ".lock" per segment, no trailing "." or "/") is
// reused via a single safeBranchName(ref) call rather than duplicated as a
// new regex; the one gap safeBranchName leaves is its leading-"-" check,
// which only looks at the very start of the whole ref, so a dash-prefixed
// INNER segment (device_id and path_key are peer/attacker-influenced here,
// unlike a locally-typed branch name) would slip through — closed below with
// an explicit per-segment check.
// isHexObjectID reports whether s looks like a full git object id: 40
// (SHA-1) or 64 (SHA-256 repo format) lowercase hex characters. Deliberately
// full-length only — PushRef's callers always hold a full id (StashCreate
// output), and abbreviations/revision expressions are not object ids.
func isHexObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func safeRefPath(ref string) bool {
	const prefix = "refs/devstrap/wip/"
	rest := strings.TrimPrefix(ref, prefix)
	if rest == ref { // prefix missing
		return false
	}
	if !strings.Contains(rest, "/") { // need a device-id segment AND >=1 path segment
		return false
	}
	if !safeBranchName(ref) {
		return false
	}
	for _, part := range strings.Split(rest, "/") {
		if strings.HasPrefix(part, "-") {
			return false
		}
	}
	return true
}

func redactGitText(value string) string {
	return urlCredentials.ReplaceAllString(value, "${1}[REDACTED]@")
}

func classifyGitError(stderr string) error {
	text := strings.ToLower(stderr)
	switch {
	case strings.Contains(text, "non-fast-forward"),
		strings.Contains(text, "fetch first"),
		strings.Contains(text, "stale info"),
		strings.Contains(text, "cannot lock ref"),
		strings.Contains(text, "[rejected]"):
		return ErrNonFastForward
	case strings.Contains(text, "couldn't find remote ref"),
		strings.Contains(text, "could not find remote ref"),
		strings.Contains(text, "remote ref does not exist"),
		strings.Contains(text, "invalid refspec"):
		return ErrBranchNotFound
	case strings.Contains(text, "no such remote"),
		strings.Contains(text, "does not appear to be a git repository"):
		return ErrRemoteMissing
	case strings.Contains(text, "authentication failed"),
		strings.Contains(text, "permission denied"),
		strings.Contains(text, "repository not found"),
		strings.Contains(text, "could not read from remote repository"):
		return ErrAuth
	case strings.Contains(text, "could not resolve host"),
		strings.Contains(text, "failed to connect"),
		strings.Contains(text, "connection timed out"),
		strings.Contains(text, "network is unreachable"),
		strings.Contains(text, "connection reset"),
		strings.Contains(text, "early eof"),
		strings.Contains(text, "the remote end hung up unexpectedly"),
		strings.Contains(text, "rpc failed"):
		return ErrNetwork
	default:
		return nil
	}
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
