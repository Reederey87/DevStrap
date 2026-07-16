package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
)

// P5-CLI-01 part B: worktree new/finalize/remove/cleanup --json shapes via the
// shared opts.render seam (worktree unlock/status/list were part A).

func TestWorktreeNewJSON(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	setupFreshWorktreeRepo(t, home, root, "auto", false)

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json",
		"worktree", "new", "work/acme/repo", "--fresh-upstream", "--name", "new json")
	if err != nil {
		t.Fatalf("worktree new --json stderr = %q err = %v", stderr, err)
	}
	var got state.Worktree
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("worktree new --json is not state.Worktree: %v\n%s", err, stdout)
	}
	if got.ID == "" || got.Branch == "" || got.Path == "" {
		t.Fatalf("worktree new --json = %+v, want id/branch/path", got)
	}
	if got.BaseRef == "" || got.BaseSHA == "" {
		t.Fatalf("worktree new --json = %+v, want base_ref and base_sha", got)
	}
	if !strings.Contains(got.Branch, "new-json") {
		t.Fatalf("worktree new --json branch = %q, want new-json slug", got.Branch)
	}
}

func TestWorktreeFinalizeJSON(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	setupFreshWorktreeRepo(t, home, root, "auto", false)

	if _, stderr, err := executeForTest("--home", home, "--root", root,
		"worktree", "new", "work/acme/repo", "--fresh-upstream", "--name", "finalize json"); err != nil {
		t.Fatalf("worktree new stderr = %q err = %v", stderr, err)
	}
	worktrees := listWorktreesForTest(t, home, root)
	if len(worktrees) != 1 {
		t.Fatalf("worktree count = %d, want 1: %+v", len(worktrees), worktrees)
	}
	wtID := worktrees[0].ID

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json",
		"worktree", "finalize", wtID)
	if err != nil {
		t.Fatalf("worktree finalize --json stderr = %q err = %v", stderr, err)
	}
	var got worktreeFinalizeResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("worktree finalize --json is not worktreeFinalizeResult: %v\n%s", err, stdout)
	}
	if got.ID != wtID {
		t.Fatalf("worktree finalize --json id = %q, want %q", got.ID, wtID)
	}
	if !got.Fresh {
		t.Fatalf("worktree finalize --json = %+v, want fresh=true", got)
	}
	if got.BaseRef == "" || got.BaseSHA == "" || got.CurrentSHA == "" {
		t.Fatalf("worktree finalize --json = %+v, want base/current shas", got)
	}
}

func TestWorktreeRemoveJSON(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	setupFreshWorktreeRepo(t, home, root, "auto", false)

	if _, stderr, err := executeForTest("--home", home, "--root", root,
		"worktree", "new", "work/acme/repo", "--fresh-upstream", "--name", "remove json"); err != nil {
		t.Fatalf("worktree new stderr = %q err = %v", stderr, err)
	}
	worktrees := listWorktreesForTest(t, home, root)
	if len(worktrees) != 1 {
		t.Fatalf("worktree count = %d, want 1: %+v", len(worktrees), worktrees)
	}
	wtID := worktrees[0].ID

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json",
		"worktree", "remove", wtID)
	if err != nil {
		t.Fatalf("worktree remove --json stderr = %q err = %v", stderr, err)
	}
	var got worktreeRemoveResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("worktree remove --json is not worktreeRemoveResult: %v\n%s", err, stdout)
	}
	if got.ID != wtID || got.Pruned {
		t.Fatalf("worktree remove --json = %+v, want id=%s pruned=false", got, wtID)
	}
}

// TestWorktreeCleanupJSONStaysPure exercises the cleanup --json contract and
// the stderr routing for non-fatal diagnostics (same class of bug as
// TestHubCompactJSONStaysPureWhenDrainingBlobs): warnings must not precede
// the JSON document on stdout.
func TestWorktreeCleanupJSONStaysPure(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	localPath := setupFreshWorktreeRepo(t, home, root, "auto", false)

	if _, stderr, err := executeForTest("--home", home, "--root", root,
		"worktree", "new", "work/acme/repo", "--fresh-upstream", "--name", "cleanup json"); err != nil {
		t.Fatalf("worktree new stderr = %q err = %v", stderr, err)
	}
	worktrees := listWorktreesForTest(t, home, root)
	if len(worktrees) != 1 {
		t.Fatalf("worktree count = %d, want 1: %+v", len(worktrees), worktrees)
	}
	wtID := worktrees[0].ID

	// Force base-refresh to fail so cleanup emits a "warning: could not refresh"
	// diagnostic, without disturbing the merge-detection check that also reads
	// wt.BaseRef: remove the origin remote's directory (git fetch then fails,
	// non-fatally, per cleanupOneWorktree's design) while leaving the
	// already-fetched local refs/remotes/origin/main ref in place, so `git
	// branch --merged origin/main` still resolves purely from local refs and
	// correctly detects the branch as merged. (An earlier version of this test
	// corrupted base_ref to a malformed string, which also broke the
	// merge-detection git command using the same ref, causing a false skip
	// instead of exercising the reap path this test is meant to cover.)
	originURL := strings.TrimSpace(runGitOutput(t, localPath, "remote", "get-url", "origin"))
	originPath := strings.TrimPrefix(originURL, "file://")
	if err := os.RemoveAll(originPath); err != nil {
		t.Fatalf("remove origin remote dir: %v", err)
	}

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json",
		"worktree", "cleanup", "--merged")
	if err != nil {
		t.Fatalf("worktree cleanup --json stderr = %q err = %v", stderr, err)
	}
	var got worktreeCleanupResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("worktree cleanup --json stdout is not a single pure worktreeCleanupResult document (warning likely leaked onto stdout): %v\nstdout=%q", err, stdout)
	}
	if got.Removed != 1 || got.Skipped != 0 {
		t.Fatalf("worktree cleanup --json = %+v, want removed=1 skipped=0", got)
	}
	if len(got.Reaped) != 1 || got.Reaped[0].ID != wtID {
		t.Fatalf("worktree cleanup --json reaped = %+v, want one entry for %s", got.Reaped, wtID)
	}
	if got.Reaped[0].MergeLabel != "merged" {
		t.Fatalf("worktree cleanup --json merge_label = %q, want merged", got.Reaped[0].MergeLabel)
	}
	if !strings.Contains(stderr, "warning: could not refresh") {
		t.Fatalf("expected base-refresh warning on stderr, got stderr=%q", stderr)
	}
	// Confirm the on-disk worktree is gone (git path still usable for branch list).
	if _, err := os.Stat(worktrees[0].Path); !os.IsNotExist(err) {
		t.Fatalf("worktree path still present after cleanup: %v", err)
	}
}
