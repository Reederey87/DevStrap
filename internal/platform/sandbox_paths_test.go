package platform

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveSandboxSpecPathsResolvesSymlinksAndToleratesMissingDenyAnchors(t *testing.T) {
	root := t.TempDir()
	realRoot := filepath.Join(root, "real")
	linkRoot := filepath.Join(root, "link")
	for _, dir := range []string{
		filepath.Join(realRoot, "worktree"),
		filepath.Join(realRoot, "tmp"),
		filepath.Join(realRoot, "logs"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Fatal(err)
	}
	realRoot, err := filepath.EvalSymlinks(realRoot)
	if err != nil {
		t.Fatal(err)
	}

	missingHome := filepath.Join(root, "missing-home")
	resolved, err := resolveSandboxSpecPaths(SandboxSpec{
		WorktreeDir: filepath.Join(linkRoot, "worktree"),
		TmpDir:      filepath.Join(linkRoot, "tmp"),
		LogDir:      filepath.Join(linkRoot, "logs"),
		UserHome:    missingHome,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.WorktreeDir != filepath.Join(realRoot, "worktree") {
		t.Fatalf("WorktreeDir = %q, want real path", resolved.WorktreeDir)
	}
	if resolved.TmpDir != filepath.Join(realRoot, "tmp") {
		t.Fatalf("TmpDir = %q, want real path", resolved.TmpDir)
	}
	if resolved.LogDir != filepath.Join(realRoot, "logs") {
		t.Fatalf("LogDir = %q, want real path", resolved.LogDir)
	}
	if resolved.UserHome != missingHome {
		t.Fatalf("UserHome = %q, want unresolved missing path %q", resolved.UserHome, missingHome)
	}

	if _, err := resolveSandboxSpecPaths(SandboxSpec{WorktreeDir: filepath.Join(root, "missing-worktree")}); err == nil {
		t.Fatal("resolveSandboxSpecPaths succeeded for missing WorktreeDir, want error")
	}
}
