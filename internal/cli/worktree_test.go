package cli

import (
	"context"
	"errors"
	"os"
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

	store, err := state.Open(filepath.Join(home, "state.db"))
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
