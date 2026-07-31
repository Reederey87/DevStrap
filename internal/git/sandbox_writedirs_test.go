package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWorktreeSandboxWriteDirs proves the resolver grants exactly the git
// storage a linked worktree writes for add/commit — objects, refs, logs, and
// the per-worktree admin dir — and NEVER the common dir itself or its
// hooks/config (which would be a sandbox escape, P7-SANDBOX-01).
func TestWorktreeSandboxWriteDirs(t *testing.T) {
	repo, r := initSquashMergeRepo(t) // real local repo on branch main with one commit
	ctx := context.Background()

	wt := filepath.Join(t.TempDir(), "wt")
	if err := r.WorktreeAdd(ctx, repo, wt, "agent/x", "main"); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}

	dirs, err := r.WorktreeSandboxWriteDirs(ctx, wt)
	if err != nil {
		t.Fatalf("WorktreeSandboxWriteDirs: %v", err)
	}
	if len(dirs) != 4 {
		t.Fatalf("want 4 grant dirs (objects/refs/logs + per-worktree admin), got %d: %v", len(dirs), dirs)
	}

	commonReal := mustEval(t, filepath.Join(repo, ".git"))
	var bases []string
	sawWorktreeAdmin := false
	for _, d := range dirs {
		// Security invariants: never the common dir root, never hooks/, never config.
		if d == commonReal {
			t.Errorf("grant includes the common dir root %q — would expose hooks/config (sandbox escape)", d)
		}
		if strings.Contains(d, string(os.PathSeparator)+"hooks") || strings.HasSuffix(d, string(os.PathSeparator)+"config") {
			t.Errorf("grant includes a hooks/config path %q", d)
		}
		bases = append(bases, filepath.Base(d))
		if strings.Contains(d, string(os.PathSeparator)+"worktrees"+string(os.PathSeparator)) {
			sawWorktreeAdmin = true
		}
	}
	for _, want := range []string{"objects", "refs", "logs"} {
		if !contains(bases, want) {
			t.Errorf("missing grant for %s; bases=%v", want, bases)
		}
	}
	if !sawWorktreeAdmin {
		t.Errorf("missing the per-worktree admin dir (…/worktrees/<name>); dirs=%v", dirs)
	}
}

// TestWorktreeSandboxWriteDirsMainCheckout covers the gitDirAbs == commonAbs
// branch: in a plain (main) checkout the grant is exactly objects/refs/logs —
// no per-worktree admin dir, and still never the common dir root or its
// hooks/config.
func TestWorktreeSandboxWriteDirsMainCheckout(t *testing.T) {
	repo, r := initSquashMergeRepo(t)
	ctx := context.Background()

	dirs, err := r.WorktreeSandboxWriteDirs(ctx, repo)
	if err != nil {
		t.Fatalf("WorktreeSandboxWriteDirs: %v", err)
	}
	if len(dirs) != 3 {
		t.Fatalf("want 3 grant dirs (objects/refs/logs) in a main checkout, got %d: %v", len(dirs), dirs)
	}

	commonReal := mustEval(t, filepath.Join(repo, ".git"))
	var bases []string
	for _, d := range dirs {
		if d == commonReal {
			t.Errorf("grant includes the common dir root %q — would expose hooks/config (sandbox escape)", d)
		}
		if strings.Contains(d, string(os.PathSeparator)+"hooks") || strings.HasSuffix(d, string(os.PathSeparator)+"config") {
			t.Errorf("grant includes a hooks/config path %q", d)
		}
		if strings.Contains(d, string(os.PathSeparator)+"worktrees"+string(os.PathSeparator)) {
			t.Errorf("main checkout must not grant a per-worktree admin dir, got %q", d)
		}
		bases = append(bases, filepath.Base(d))
	}
	for _, want := range []string{"objects", "refs", "logs"} {
		if !contains(bases, want) {
			t.Errorf("missing grant for %s; bases=%v", want, bases)
		}
	}
}

// TestWorktreeSandboxWriteDirsNonRepo returns (nil, nil) outside a git worktree
// so the caller grants nothing without special-casing.
func TestWorktreeSandboxWriteDirsNonRepo(t *testing.T) {
	r := Runner{Bin: gitBinOrSkip(t), Timeout: 5 * time.Second}
	dirs, err := r.WorktreeSandboxWriteDirs(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dirs != nil {
		t.Fatalf("want nil grant outside a git worktree, got %v", dirs)
	}
}

// TestWorktreeSandboxDenyFiles proves the P8-SEC-02 deny list names exactly
// the three git-dir pointer files (commondir, gitdir, config.worktree) inside
// a linked worktree's per-worktree admin dir — the files
// WorktreeSandboxWriteDirs grants writable wholesale but that must stay
// read-only, since rewriting commondir relocates git's whole configuration
// into attacker-controlled space.
func TestWorktreeSandboxDenyFiles(t *testing.T) {
	repo, r := initSquashMergeRepo(t)
	ctx := context.Background()

	wt := filepath.Join(t.TempDir(), "wt")
	if err := r.WorktreeAdd(ctx, repo, wt, "agent/deny", "main"); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}

	writeDirs, err := r.WorktreeSandboxWriteDirs(ctx, wt)
	if err != nil {
		t.Fatalf("WorktreeSandboxWriteDirs: %v", err)
	}
	denyFiles, err := r.WorktreeSandboxDenyFiles(ctx, wt)
	if err != nil {
		t.Fatalf("WorktreeSandboxDenyFiles: %v", err)
	}
	if len(denyFiles) != 3 {
		t.Fatalf("want 3 deny files (commondir, gitdir, config.worktree), got %d: %v", len(denyFiles), denyFiles)
	}
	var bases []string
	for _, f := range denyFiles {
		bases = append(bases, filepath.Base(f))
	}
	for _, want := range []string{"commondir", "gitdir", "config.worktree"} {
		if !contains(bases, want) {
			t.Errorf("missing deny file %s; bases=%v", want, bases)
		}
	}
	// Every deny file must live inside one of the granted per-worktree admin
	// write dirs — otherwise the deny would be denying a path that was never
	// writable in the first place, proving nothing.
	var worktreeAdmin string
	for _, d := range writeDirs {
		if strings.Contains(d, string(os.PathSeparator)+"worktrees"+string(os.PathSeparator)) {
			worktreeAdmin = d
		}
	}
	if worktreeAdmin == "" {
		t.Fatal("no per-worktree admin dir found in write grants; cannot verify deny files are inside it")
	}
	for _, f := range denyFiles {
		if filepath.Dir(f) != worktreeAdmin {
			t.Errorf("deny file %q is not inside the granted admin dir %q", f, worktreeAdmin)
		}
	}
}

// TestWorktreeSandboxDenyFilesMainCheckoutAndNonRepo pins the nil-for-non-linked
// contract: a main checkout (no per-worktree admin dir exists to protect) and a
// plain non-repo dir both yield an empty deny list with no error, mirroring
// WorktreeSandboxWriteDirs.
func TestWorktreeSandboxDenyFilesMainCheckoutAndNonRepo(t *testing.T) {
	repo, r := initSquashMergeRepo(t)
	ctx := context.Background()

	denyFiles, err := r.WorktreeSandboxDenyFiles(ctx, repo)
	if err != nil {
		t.Fatalf("WorktreeSandboxDenyFiles (main checkout): %v", err)
	}
	if denyFiles != nil {
		t.Fatalf("want nil deny list for the main checkout, got %v", denyFiles)
	}

	r2 := Runner{Bin: gitBinOrSkip(t), Timeout: 5 * time.Second}
	denyFiles, err = r2.WorktreeSandboxDenyFiles(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("WorktreeSandboxDenyFiles (non-repo): %v", err)
	}
	if denyFiles != nil {
		t.Fatalf("want nil deny list outside a git worktree, got %v", denyFiles)
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return filepath.Clean(p)
	}
	return r
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func gitBinOrSkip(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	return bin
}

// TestWorktreeSandboxWriteDirsSeparateGitDir covers the input class the other
// three cases in this file do not: a checkout whose git-common-dir is NOT
// "<checkout>/.git" because it was made with `--separate-git-dir`. A linked
// worktree of such a repo still needs its git-storage writes granted, or the
// agent's own `git add`/`git commit` fail with EPERM under the OS sandbox.
//
// This exists because extracting WorktreeIdentity briefly made the main-checkout
// derivation FATAL, which this function translated into its nil,nil "grant
// nothing" contract — silently denying every git write for these layouts. The
// three pre-existing cases all passed throughout, which is exactly why an
// unchanged-and-green test file proved nothing about this input.
func TestWorktreeSandboxWriteDirsSeparateGitDir(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	checkout := filepath.Join(base, "checkout")
	gitDir := filepath.Join(base, "elsewhere.git")
	r := NewRunner()

	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	run(base, "init", "--separate-git-dir", gitDir, "-b", "main", checkout)
	if err := os.WriteFile(filepath.Join(checkout, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(checkout, "add", "f.txt")
	run(checkout, "commit", "-m", "initial")

	wt := filepath.Join(base, "wt")
	if err := r.WorktreeAdd(ctx, checkout, wt, "agent/sep", "main"); err != nil {
		t.Fatalf("WorktreeAdd on a --separate-git-dir repo: %v", err)
	}

	dirs, err := r.WorktreeSandboxWriteDirs(ctx, wt)
	if err != nil {
		t.Fatalf("WorktreeSandboxWriteDirs: %v", err)
	}
	if len(dirs) == 0 {
		t.Fatal("granted NO write dirs for a linked worktree of a --separate-git-dir repo; " +
			"the sandboxed agent's `git commit` would fail with EPERM")
	}
	var haveObjects, haveRefs bool
	for _, d := range dirs {
		switch filepath.Base(d) {
		case "objects":
			haveObjects = true
		case "refs":
			haveRefs = true
		}
	}
	if !haveObjects || !haveRefs {
		t.Fatalf("granted dirs must include the shared object store and refs; got %v", dirs)
	}
}
