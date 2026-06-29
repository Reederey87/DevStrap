package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	Bin           string
	Timeout       time.Duration
	RetryAttempts int
	RetryBackoff  time.Duration
}

func NewRunner() Runner {
	return Runner{Bin: "git", Timeout: 2 * time.Minute, RetryAttempts: 3, RetryBackoff: 200 * time.Millisecond}
}

var (
	ErrNetwork        = errors.New("git network error")
	ErrAuth           = errors.New("git authentication error")
	ErrBranchNotFound = errors.New("git branch not found")
	ErrRemoteMissing  = errors.New("git remote missing")
)

type CommandError struct {
	Kind    error
	Args    string
	Message string
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

func (r Runner) Run(ctx context.Context, dir string, args ...string) (string, error) {
	bin := r.Bin
	if bin == "" {
		bin = "git"
	}
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	if _, ok := ctx.Deadline(); !ok && timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
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
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", CommandError{Kind: ErrNetwork, Args: argText, Message: fmt.Sprintf("timed out after %s", timeout)}
		}
		return "", CommandError{Kind: kind, Args: argText, Message: msg}
	}
	return strings.TrimSpace(stdout.String()), nil
}

func (r Runner) Clone(ctx context.Context, remote, dest string, partial bool) error {
	if err := ValidateRemote(remote); err != nil {
		return err
	}
	args := cloneArgs(remote, dest, partial)
	attempts := r.RetryAttempts
	if attempts <= 0 {
		attempts = 1
	}
	backoff := r.RetryBackoff
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
		_, err := r.Run(ctx, "", args...)
		if err == nil {
			return nil
		}
		lastErr = err
		if !errors.Is(err, ErrNetwork) || attempt == attempts {
			return err
		}
		if err := sleepBackoff(ctx, backoff, attempt); err != nil {
			return err
		}
	}
	return lastErr
}

// cloneArgs builds the argv for a git clone with optional blobless partial
// clone (GIT-02).
func cloneArgs(remote, dest string, partial bool) []string {
	args := []string{"clone"}
	if partial {
		args = append(args, "--filter=blob:none")
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
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		_, err := r.Run(ctx, dir, args...)
		if err == nil {
			return nil
		}
		lastErr = err
		if !errors.Is(err, ErrNetwork) || attempt == attempts {
			return err
		}
		if err := sleepBackoff(ctx, backoff, attempt); err != nil {
			return err
		}
	}
	return lastErr
}

// sleepBackoff waits for the configured network-retry backoff (linear in the
// attempt number) or until ctx is cancelled. A non-positive backoff returns
// immediately so the next attempt runs without delay. QUAL-06 will replace this
// with capped exponential backoff plus full jitter.
func sleepBackoff(ctx context.Context, backoff time.Duration, attempt int) error {
	if backoff <= 0 {
		return nil
	}
	timer := time.NewTimer(backoff * time.Duration(attempt))
	select {
	case <-ctx.Done():
		timer.Stop()
		return ctx.Err()
	case <-timer.C:
		return nil
	}
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

func (r Runner) symbolicOriginHead(ctx context.Context, dir string) (string, bool) {
	out, err := r.Run(ctx, dir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if err != nil {
		return "", false
	}
	branch := strings.TrimPrefix(strings.TrimSpace(out), "origin/")
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
		branch := strings.TrimPrefix(fields[0], "refs/heads/")
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
	_, err := r.Run(ctx, dir, "lfs", "pull")
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

func redactGitText(value string) string {
	return urlCredentials.ReplaceAllString(value, "${1}[REDACTED]@")
}

func classifyGitError(stderr string) error {
	text := strings.ToLower(stderr)
	switch {
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
