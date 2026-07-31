package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestWorktreeIdentityMainCheckout covers the plain (main) checkout case:
// IsLinked is false, MainCheckout is the repo itself, and Branch/HeadSHA
// reflect the checked-out branch tip.
func TestWorktreeIdentityMainCheckout(t *testing.T) {
	repo, r := initSquashMergeRepo(t)
	ctx := context.Background()

	id, err := r.WorktreeIdentity(ctx, repo)
	if err != nil {
		t.Fatalf("WorktreeIdentity: %v", err)
	}
	if id.IsLinked {
		t.Fatalf("main checkout must not report IsLinked")
	}
	wantMain := mustEval(t, repo)
	if id.MainCheckout != wantMain {
		t.Fatalf("MainCheckout = %q, want %q", id.MainCheckout, wantMain)
	}
	if id.Branch != "main" {
		t.Fatalf("Branch = %q, want main", id.Branch)
	}
	if id.HeadSHA == "" {
		t.Fatalf("HeadSHA must not be empty for a committed repo")
	}
	if id.CommonDir != id.GitDir {
		t.Fatalf("main checkout must have CommonDir == GitDir, got %q vs %q", id.CommonDir, id.GitDir)
	}
}

// TestWorktreeIdentityLinkedWorktree covers a genuine linked worktree created
// by `git worktree add`: IsLinked is true, MainCheckout resolves back to the
// original repo, and Branch/HeadSHA reflect the worktree's own checkout.
func TestWorktreeIdentityLinkedWorktree(t *testing.T) {
	repo, r := initSquashMergeRepo(t)
	ctx := context.Background()

	wt := filepath.Join(t.TempDir(), "linked-wt")
	if err := r.WorktreeAdd(ctx, repo, wt, "agent/linked", "main"); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}

	id, err := r.WorktreeIdentity(ctx, wt)
	if err != nil {
		t.Fatalf("WorktreeIdentity: %v", err)
	}
	if !id.IsLinked {
		t.Fatalf("linked worktree must report IsLinked")
	}
	wantMain := mustEval(t, repo)
	if id.MainCheckout != wantMain {
		t.Fatalf("MainCheckout = %q, want %q", id.MainCheckout, wantMain)
	}
	if id.Branch != "agent/linked" {
		t.Fatalf("Branch = %q, want agent/linked", id.Branch)
	}
	if id.HeadSHA == "" {
		t.Fatalf("HeadSHA must not be empty")
	}
}

// TestWorktreeIdentityDetachedHead covers the common agent-harness case: a
// linked worktree checked out at a detached commit. Branch must be "" with NO
// error — this is the expected, adoptable shape, not a failure.
func TestWorktreeIdentityDetachedHead(t *testing.T) {
	repo, r := initSquashMergeRepo(t)
	ctx := context.Background()
	gitBin := r.Bin

	head, err := r.RevParse(ctx, repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), "detached-wt")
	runRealGit(t, gitBin, repo, "worktree", "add", "--detach", wt, head)

	id, err := r.WorktreeIdentity(ctx, wt)
	if err != nil {
		t.Fatalf("WorktreeIdentity: %v", err)
	}
	if !id.IsLinked {
		t.Fatalf("detached linked worktree must still report IsLinked")
	}
	if id.Branch != "" {
		t.Fatalf("Branch = %q, want empty for detached HEAD", id.Branch)
	}
	if id.HeadSHA != head {
		t.Fatalf("HeadSHA = %q, want %q", id.HeadSHA, head)
	}
}

// TestWorktreeIdentityUnbornHead covers a freshly `git init`ed repo with no
// commits: rev-parse --verify HEAD fails, so HeadSHA must be "" with NO error
// (unborn is a legitimate, distinguishable state — callers refuse adoption on
// it, but resolving identity itself must not fail).
func TestWorktreeIdentityUnbornHead(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	runRealGit(t, gitBin, repo, "init")
	r := Runner{Bin: gitBin, Timeout: 5 * time.Second}

	id, err := r.WorktreeIdentity(context.Background(), repo)
	if err != nil {
		t.Fatalf("WorktreeIdentity: %v", err)
	}
	if id.HeadSHA != "" {
		t.Fatalf("HeadSHA = %q, want empty for unborn HEAD", id.HeadSHA)
	}
}

// TestWorktreeIdentityNonRepo returns a real error (never nil,nil — that
// translation is WorktreeSandboxWriteDirs' contract, not WorktreeIdentity's).
func TestWorktreeIdentityNonRepo(t *testing.T) {
	r := Runner{Bin: gitBinOrSkip(t), Timeout: 5 * time.Second}
	_, err := r.WorktreeIdentity(context.Background(), t.TempDir())
	if err == nil {
		t.Fatalf("want an error outside a git worktree")
	}
}

// TestWorktreeIdentityBareRepo covers a bare repo whose git-common-dir does
// not end in ".git": guessing a main checkout would be wrong, so this must
// return an explicit error rather than fabricating a path.
func TestWorktreeIdentityBareRepo(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	// Deliberately NOT named "*.git" so git-common-dir ("." resolved to this
	// dir) cannot coincidentally end in ".git".
	bare := filepath.Join(t.TempDir(), "bare-repo")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	runRealGit(t, gitBin, bare, "init", "--bare")
	r := Runner{Bin: gitBin, Timeout: 5 * time.Second}

	_, err = r.WorktreeIdentity(context.Background(), bare)
	if err == nil {
		t.Fatalf("want an error for a bare repo (git-common-dir does not end in .git)")
	}
}

func TestMergeBaseResolvesCommonAncestor(t *testing.T) {
	repo, r := initSquashMergeRepo(t)
	ctx := context.Background()
	gitBin := r.Bin

	base, err := r.RevParse(ctx, repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	runRealGit(t, gitBin, repo, "checkout", "-b", "feature")
	writeAndCommit(t, gitBin, repo, "feature.txt", "feature\n", "feature commit")

	got, err := r.MergeBase(ctx, repo, "main", "feature")
	if err != nil {
		t.Fatalf("MergeBase: %v", err)
	}
	if got != base {
		t.Fatalf("MergeBase = %q, want %q", got, base)
	}
}

// TestMergeBaseNoCommonAncestor pins the ErrNoMergeBase sentinel for two
// orphan (unrelated-history) branches — git's documented exit-1/empty-output
// outcome, which is an expected result (a gh-pages branch is the common real
// case), not an operational failure.
func TestMergeBaseNoCommonAncestor(t *testing.T) {
	repo, r := initSquashMergeRepo(t)
	ctx := context.Background()
	gitBin := r.Bin

	runRealGit(t, gitBin, repo, "checkout", "--orphan", "gh-pages")
	runRealGit(t, gitBin, repo, "rm", "-rf", "--cached", ".")
	writeAndCommit(t, gitBin, repo, "index.html", "hi\n", "orphan commit")

	_, err := r.MergeBase(ctx, repo, "main", "gh-pages")
	if !errors.Is(err, ErrNoMergeBase) {
		t.Fatalf("MergeBase err = %v, want ErrNoMergeBase", err)
	}
}

func TestIsShallowFalseForFullClone(t *testing.T) {
	repo, r := initSquashMergeRepo(t)
	shallow, err := r.IsShallow(context.Background(), repo)
	if err != nil {
		t.Fatalf("IsShallow: %v", err)
	}
	if shallow {
		t.Fatalf("IsShallow = true, want false for a full local repo")
	}
}

func TestIsShallowTrueForShallowClone(t *testing.T) {
	repo, r := initSquashMergeRepo(t)
	gitBin := r.Bin
	// Two commits so a --depth 1 clone is meaningfully shallow.
	writeAndCommit(t, gitBin, repo, "second.txt", "second\n", "second commit")

	shallowClone := filepath.Join(t.TempDir(), "shallow-clone")
	// "--depth is ignored in local clones" unless the source is a file://
	// URL — a plain path clone silently produces a full (non-shallow) clone.
	runRealGit(t, gitBin, "", "clone", "--depth", "1", "file://"+repo, shallowClone)

	shallow, err := r.IsShallow(context.Background(), shallowClone)
	if err != nil {
		t.Fatalf("IsShallow: %v", err)
	}
	if !shallow {
		t.Fatalf("IsShallow = false, want true for a --depth 1 clone")
	}
}
