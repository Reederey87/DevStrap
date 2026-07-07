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
