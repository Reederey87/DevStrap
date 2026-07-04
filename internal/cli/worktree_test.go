package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/state"
)

type fakeWorktreeAdder struct {
	calls    int
	branches []string
}

func (f *fakeWorktreeAdder) WorktreeAdd(_ context.Context, _, _, branch, _ string) error {
	f.calls++
	f.branches = append(f.branches, branch)
	if strings.Contains(branch, "collision") {
		return errors.New("fatal: a branch named 'collision' already exists")
	}
	return nil
}

func TestAddWorktreeWithFreshBranchRetriesBranchCollision(t *testing.T) {
	oldNow := worktreeNow
	oldSuffix := worktreeSuffixFunc
	t.Cleanup(func() {
		worktreeNow = oldNow
		worktreeSuffixFunc = oldSuffix
	})
	worktreeNow = func() time.Time {
		return time.Date(2026, 6, 25, 12, 34, 56, 0, time.UTC)
	}
	suffixes := []string{"collision", "unique"}
	worktreeSuffixFunc = func(int) (string, error) {
		next := suffixes[0]
		suffixes = suffixes[1:]
		return next, nil
	}

	adder := &fakeWorktreeAdder{}
	branch, wtPath, err := addWorktreeWithFreshBranch(t.Context(), adder, t.TempDir(), "prj_test", "/repo", "fix-tests", "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if adder.calls != 2 {
		t.Fatalf("calls = %d, want retry after one collision", adder.calls)
	}
	if got, want := adder.branches[0], "agent/fix-tests-20260625-123456-collision"; got != want {
		t.Fatalf("first branch = %q, want %q", got, want)
	}
	if got, want := branch, "agent/fix-tests-20260625-123456-unique"; got != want {
		t.Fatalf("branch = %q, want %q", got, want)
	}
	if !strings.HasSuffix(wtPath, filepath.Join("prj_test", "agent-fix-tests-20260625-123456-unique")) {
		t.Fatalf("worktree path = %q, want branch-derived suffix", wtPath)
	}
}

func TestCreateFreshWorktreeCleansUpAfterLFSPullFailure(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	localPath := setupFreshWorktreeRepo(t, home, root, "auto", true)
	setProjectLFSPolicy(t, filepath.Join(home, "state.db"), "work/acme/repo", "always")
	installFailingGitLFS(t)

	_, stderr, err := executeForTest("--home", home, "--root", root, "worktree", "new", "work/acme/repo", "--fresh-upstream", "--name", "lfs failure")
	if err == nil {
		t.Fatal("expected LFS pull failure")
	}
	wtPath := lfsFailureWorktreePath(t, stderr)
	if !strings.Contains(stderr, wtPath) {
		t.Fatalf("stderr = %q, want worktree path %q", stderr, wtPath)
	}
	assertNoOrphanWorktree(t, localPath, wtPath)
}

func TestCreateFreshWorktreeCleansUpAfterInsertWorktreeFailure(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	localPath := setupFreshWorktreeRepo(t, home, root, "auto", false)
	installFailingWorktreeInsertTrigger(t, filepath.Join(home, "state.db"))

	_, stderr, err := executeForTest("--home", home, "--root", root, "worktree", "new", "work/acme/repo", "--fresh-upstream", "--name", "insert failure")
	if err == nil {
		t.Fatal("expected worktree insert failure")
	}
	if !strings.Contains(stderr, "forced worktree insert failure") {
		t.Fatalf("stderr = %q, want forced insert failure", stderr)
	}
	matches, err := filepath.Glob(filepath.Join(home, "worktrees", "*", "agent-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("leftover worktree directories = %v", matches)
	}
	assertNoAgentBranches(t, localPath)
}

func TestWorktreeUnlockClearsStaleAndRefusesLiveLock(t *testing.T) {
	ctx := context.Background()
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "add", "git@github.com:acme/api.git", "--path", "work/acme/api"); err != nil {
		t.Fatalf("add stderr = %q err = %v", stderr, err)
	}

	store, err := state.Open(context.Background(), filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.ProjectByPath(ctx, "work/acme/api")
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	lockDir := filepath.Join(home, "locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(lockDir, project.ID+".lock")

	// A live lock (current process) must be refused without --force.
	liveInfo := `{"pid":` + itoa(os.Getpid()) + `,"hostname":"` + hostname() + `","acquired_at":"` + time.Now().UTC().Format(time.RFC3339Nano) + `"}`
	if err := os.WriteFile(lockPath, []byte(liveInfo), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := executeForTest("--home", home, "--root", root, "worktree", "unlock", "work/acme/api"); err == nil {
		t.Fatal("expected live lock to be refused without --force")
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("live lock was removed without --force: %v", err)
	}

	// A stale lock (dead pid) must be reported and cleared.
	oldAlive := repoLockProcessAlive
	repoLockProcessAlive = func(int) bool { return false }
	t.Cleanup(func() { repoLockProcessAlive = oldAlive })
	staleInfo := `{"pid":999999,"hostname":"` + hostname() + `","acquired_at":"` + time.Now().UTC().Format(time.RFC3339Nano) + `"}`
	if err := os.WriteFile(lockPath, []byte(staleInfo), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "worktree", "unlock", "work/acme/api")
	if err != nil {
		t.Fatalf("unlock stale stderr = %q err = %v", stderr, err)
	}
	if !strings.Contains(stdout, "Cleared") {
		t.Fatalf("unlock stdout = %q, want cleared message", stdout)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("stale lock was not cleared: %v", err)
	}
}

func itoa(n int) string {
	return strconv.Itoa(n)
}

func setupFreshWorktreeRepo(t *testing.T, home, root, lfsPolicy string, usesLFS bool) string {
	t.Helper()
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}

	tmp := t.TempDir()
	remote := filepath.Join(tmp, "repo.git")
	seed := filepath.Join(tmp, "seed")
	runGit(t, tmp, "init", "--bare", remote)
	runGit(t, seed, "init")
	runGit(t, seed, "config", "user.email", "devstrap@example.test")
	runGit(t, seed, "config", "user.name", "DevStrap Test")
	runGit(t, seed, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := []string{"README.md"}
	if usesLFS {
		if err := os.WriteFile(filepath.Join(seed, ".gitattributes"), []byte("*.bin filter=lfs diff=lfs merge=lfs -text\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		files = append(files, ".gitattributes")
	}
	runGit(t, seed, append([]string{"add"}, files...)...)
	runGit(t, seed, "commit", "-m", "initial")
	runGit(t, seed, "remote", "add", "origin", remote)
	runGit(t, seed, "push", "origin", "main")
	runGit(t, tmp, "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/main")

	if _, stderr, err := executeForTest("--home", home, "--root", root, "add", "file://"+remote, "--path", "work/acme/repo", "--default-branch", "main", "--lfs-policy", lfsPolicy); err != nil {
		t.Fatalf("add stderr = %q err = %v", stderr, err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "hydrate", "work/acme/repo"); err != nil {
		t.Fatalf("hydrate stderr = %q err = %v", stderr, err)
	}
	return filepath.Join(root, "work", "acme", "repo")
}

func installFailingGitLFS(t *testing.T) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	fakeBin := t.TempDir()
	fakeGit := filepath.Join(fakeBin, "git")
	script := fmt.Sprintf(`#!/bin/sh
prev=
for arg in "$@"; do
	if [ "$prev" = "lfs" ] && [ "$arg" = "pull" ]; then
		echo "forced lfs failure" >&2
		exit 42
	fi
	prev=$arg
done
exec %q "$@"
`, realGit)
	if err := os.WriteFile(fakeGit, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func setProjectLFSPolicy(t *testing.T, dbPath, nsPath, policy string) {
	t.Helper()
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	dsn := (&url.URL{Scheme: "file", Path: dbPath, RawQuery: q.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	res, err := db.Exec(`
UPDATE git_repos
SET lfs_policy = ?
WHERE namespace_id = (SELECT id FROM namespace_entries WHERE path = ?);
`, policy, nsPath)
	if err != nil {
		t.Fatal(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("updated %d git_repos rows, want 1", n)
	}
}

func installFailingWorktreeInsertTrigger(t *testing.T, dbPath string) {
	t.Helper()
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	dsn := (&url.URL{Scheme: "file", Path: dbPath, RawQuery: q.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	_, err = db.Exec(`
CREATE TRIGGER fail_worktree_insert
BEFORE INSERT ON worktrees
BEGIN
	SELECT RAISE(ABORT, 'forced worktree insert failure');
END;
`)
	if err != nil {
		t.Fatal(err)
	}
}

func lfsFailureWorktreePath(t *testing.T, stderr string) string {
	t.Helper()
	const prefix = "worktree created at "
	start := strings.Index(stderr, prefix)
	if start < 0 {
		t.Fatalf("stderr = %q, want LFS failure path", stderr)
	}
	start += len(prefix)
	end := strings.Index(stderr[start:], " but LFS pull failed")
	if end < 0 {
		t.Fatalf("stderr = %q, want LFS failure suffix", stderr)
	}
	return stderr[start : start+end]
}

func assertNoOrphanWorktree(t *testing.T, localPath, wtPath string) {
	t.Helper()
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Fatalf("worktree path still exists: %s err=%v", wtPath, err)
	}
	assertNoAgentBranches(t, localPath)
}

func assertNoAgentBranches(t *testing.T, localPath string) {
	t.Helper()
	if branches := strings.TrimSpace(runGitOutput(t, localPath, "branch", "--list", "agent/*")); branches != "" {
		t.Fatalf("agent branches remain:\n%s", branches)
	}
}
