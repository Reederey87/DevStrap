package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
)

// adoptResultForTest mirrors worktreeAdoptResult's --json shape for decoding
// in tests.
type adoptResultForTest struct {
	state.Worktree
	ProjectPath       string   `json:"project_path"`
	AlreadyAdopted    bool     `json:"already_adopted,omitempty"`
	AlreadyRegistered bool     `json:"already_registered,omitempty"`
	Warnings          []string `json:"warnings,omitempty"`
}

func worktreeRowCountForTest(t *testing.T, dbPath string) int {
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
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM worktrees WHERE status = 'active'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// TestWorktreeAdoptDetachedHeadRegistersWithMergeBase covers the common
// agent-harness shape: an externally-created worktree checked out detached.
// It must be ADOPTED (never refused), with Branch == "" and a base_sha
// resolved via merge-base rather than the base ref's raw tip.
func TestWorktreeAdoptDetachedHeadRegistersWithMergeBase(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	localPath := setupFreshWorktreeRepo(t, home, root, "auto", false)

	head := strings.TrimSpace(runGitOutput(t, localPath, "rev-parse", "HEAD"))
	extWT := filepath.Join(t.TempDir(), "external-wt")
	runGit(t, localPath, "worktree", "add", "--detach", extWT, head)

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "worktree", "adopt", extWT, "--json")
	if err != nil {
		t.Fatalf("adopt stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	var out adoptResultForTest
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("decode adopt --json: %v\n%s", err, stdout)
	}
	if out.Branch != "" {
		t.Fatalf("Branch = %q, want empty for a detached-HEAD adopt", out.Branch)
	}
	if out.CreatedBy != "adopted" {
		t.Fatalf("CreatedBy = %q, want adopted", out.CreatedBy)
	}
	if out.BaseSHA != head {
		t.Fatalf("BaseSHA = %q, want %q (merge-base of HEAD with itself, since the worktree is at the branch tip)", out.BaseSHA, head)
	}
	if out.ProjectPath != "work/acme/repo" {
		t.Fatalf("ProjectPath = %q, want work/acme/repo", out.ProjectPath)
	}
	if out.AlreadyAdopted || out.AlreadyRegistered {
		t.Fatalf("first adopt must not report an idempotency marker: %+v", out)
	}
}

func TestWorktreeAdoptRefusesMainCheckout(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	localPath := setupFreshWorktreeRepo(t, home, root, "auto", false)

	_, stderr, err := executeForTest("--home", home, "--root", root, "worktree", "adopt", localPath)
	if err == nil {
		t.Fatal("want an error adopting the main checkout")
	}
	if !strings.Contains(stderr, "main checkout") {
		t.Fatalf("stderr = %q, want a main-checkout refusal", stderr)
	}
}

func TestWorktreeAdoptRefusesNonGitDirectory(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	_ = setupFreshWorktreeRepo(t, home, root, "auto", false)

	notAGitDir := t.TempDir()
	_, stderr, err := executeForTest("--home", home, "--root", root, "worktree", "adopt", notAGitDir)
	if err == nil {
		t.Fatal("want an error adopting a non-git directory")
	}
	if !strings.Contains(stderr, "not a git worktree") {
		t.Fatalf("stderr = %q, want a not-a-git-worktree refusal", stderr)
	}
}

// TestWorktreeAdoptRefusesNoCommonAncestorButBaseRefRemedyWorks pins the
// ErrNoMergeBase refusal (adopting an orphan branch such as gh-pages against
// the wrong default branch) and proves the documented --base-ref remedy
// actually resolves it.
func TestWorktreeAdoptRefusesNoCommonAncestorButBaseRefRemedyWorks(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	localPath := setupFreshWorktreeRepo(t, home, root, "auto", false)

	runGit(t, localPath, "checkout", "--orphan", "gh-pages")
	runGit(t, localPath, "rm", "-rf", "--cached", ".")
	// rm --cached leaves README.md untracked on disk; clean it before
	// committing the orphan branch so switching back to main does not refuse
	// with "untracked working tree files would be overwritten".
	runGit(t, localPath, "clean", "-fd")
	if err := os.WriteFile(filepath.Join(localPath, "index.html"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, localPath, "add", "index.html")
	runGit(t, localPath, "commit", "-m", "orphan commit")
	runGit(t, localPath, "push", "origin", "gh-pages")
	runGit(t, localPath, "checkout", "main")

	extWT := filepath.Join(t.TempDir(), "gh-pages-wt")
	runGit(t, localPath, "worktree", "add", extWT, "gh-pages")

	_, stderr, err := executeForTest("--home", home, "--root", root, "worktree", "adopt", extWT)
	if err == nil {
		t.Fatal("want a no-common-ancestor refusal against the default branch")
	}
	if !strings.Contains(stderr, "--base-ref") {
		t.Fatalf("stderr = %q, want the --base-ref remedy named", stderr)
	}

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "worktree", "adopt", extWT, "--base-ref", "origin/gh-pages", "--json")
	if err != nil {
		t.Fatalf("adopt with --base-ref stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	var out adoptResultForTest
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("decode adopt --json: %v\n%s", err, stdout)
	}
	if out.BaseRef != "origin/gh-pages" {
		t.Fatalf("BaseRef = %q, want origin/gh-pages", out.BaseRef)
	}
}

// TestWorktreeAdoptRefusesShallowCloneUnlessAllowed proves a shallow clone
// refuses (its merge-base could be wrong at the shallow boundary) and that
// --allow-shallow adopts anyway with a warning.
func TestWorktreeAdoptRefusesShallowCloneUnlessAllowed(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr=%q err=%v", stderr, err)
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
	runGit(t, seed, "add", "README.md")
	runGit(t, seed, "commit", "-m", "first")
	if err := os.WriteFile(filepath.Join(seed, "second.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "add", "second.txt")
	runGit(t, seed, "commit", "-m", "second")
	runGit(t, seed, "remote", "add", "origin", remote)
	runGit(t, seed, "push", "origin", "main")
	runGit(t, tmp, "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/main")

	projectDir := filepath.Join(root, "work", "acme", "shallow-repo")
	// "--depth is ignored in local clones" unless the source is a file://
	// URL — a plain path clone silently produces a full (non-shallow) clone.
	runGit(t, tmp, "clone", "--depth", "1", "file://"+remote, projectDir)

	if _, stderr, err := executeForTest("--home", home, "--root", root, "scan", root, "--adopt"); err != nil {
		t.Fatalf("scan --adopt stderr=%q err=%v", stderr, err)
	}

	extWT := filepath.Join(t.TempDir(), "shallow-wt")
	runGit(t, projectDir, "worktree", "add", "--detach", extWT, "HEAD")

	_, stderr, err := executeForTest("--home", home, "--root", root, "worktree", "adopt", extWT)
	if err == nil {
		t.Fatal("want a shallow-clone refusal")
	}
	if !strings.Contains(stderr, "shallow") || !strings.Contains(stderr, "--allow-shallow") {
		t.Fatalf("stderr = %q, want a shallow refusal naming --allow-shallow", stderr)
	}

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "worktree", "adopt", extWT, "--allow-shallow", "--json")
	if err != nil {
		t.Fatalf("adopt --allow-shallow stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	var out adoptResultForTest
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("decode adopt --json: %v\n%s", err, stdout)
	}
	if len(out.Warnings) == 0 {
		t.Fatalf("adopt --allow-shallow must record a warning, got %+v", out)
	}
}

// TestWorktreeAdoptReAdoptRefreshesInPlace covers the already_adopted
// idempotency path: re-running adopt on the same path after the remote
// advances refreshes base_ref/base_sha in place rather than creating a
// second row.
func TestWorktreeAdoptReAdoptRefreshesInPlace(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	localPath := setupFreshWorktreeRepo(t, home, root, "auto", false)

	head := strings.TrimSpace(runGitOutput(t, localPath, "rev-parse", "HEAD"))
	extWT := filepath.Join(t.TempDir(), "external-wt")
	runGit(t, localPath, "worktree", "add", "--detach", extWT, head)

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "worktree", "adopt", extWT, "--json")
	if err != nil {
		t.Fatalf("first adopt stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	var first adoptResultForTest
	if err := json.Unmarshal([]byte(stdout), &first); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err = executeForTest("--home", home, "--root", root, "worktree", "adopt", extWT, "--json")
	if err != nil {
		t.Fatalf("second adopt stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	var second adoptResultForTest
	if err := json.Unmarshal([]byte(stdout), &second); err != nil {
		t.Fatal(err)
	}
	if !second.AlreadyAdopted {
		t.Fatalf("second adopt = %+v, want already_adopted", second)
	}
	if second.ID != first.ID {
		t.Fatalf("second adopt ID = %q, want same ID %q (refresh in place, not a new row)", second.ID, first.ID)
	}
	if got := worktreeRowCountForTest(t, filepath.Join(home, "state.db")); got != 1 {
		t.Fatalf("active worktree row count = %d, want 1 (no duplicate row)", got)
	}
}

// TestWorktreeAdoptOnWorktreeNewRowIsAlreadyRegisteredAndMutatesNothing
// proves item 6's normalization-parity requirement end to end: `worktree
// new` and `worktree adopt` on the SAME physical worktree must hit the same
// idx_worktrees_active_path row (both write paths resolve symlinks
// identically) instead of colliding on a duplicate INSERT, and adopting a
// row created by `worktree new` must leave that row's base untouched.
func TestWorktreeAdoptOnWorktreeNewRowIsAlreadyRegisteredAndMutatesNothing(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	_ = setupFreshWorktreeRepo(t, home, root, "auto", false)

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "worktree", "new", "work/acme/repo", "--fresh-upstream", "--name", "parity-check")
	if err != nil {
		t.Fatalf("worktree new stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	worktrees := listWorktreesForTest(t, home, root)
	if len(worktrees) != 1 {
		t.Fatalf("worktree count = %d, want 1: %+v", len(worktrees), worktrees)
	}
	created := worktrees[0]

	stdout, stderr, err = executeForTest("--home", home, "--root", root, "worktree", "adopt", created.Path, "--json")
	if err != nil {
		t.Fatalf("adopt stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	var out adoptResultForTest
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("decode adopt --json: %v\n%s", err, stdout)
	}
	if !out.AlreadyRegistered {
		t.Fatalf("adopt on a `worktree new` row = %+v, want already_registered", out)
	}
	if out.ID != created.ID {
		t.Fatalf("adopt ID = %q, want the SAME row %q created by `worktree new` (path normalization parity)", out.ID, created.ID)
	}
	if out.CreatedBy != "agent" {
		t.Fatalf("CreatedBy = %q, want unchanged agent (adopt must mutate nothing on a non-adopted row)", out.CreatedBy)
	}
	if got := worktreeRowCountForTest(t, filepath.Join(home, "state.db")); got != 1 {
		t.Fatalf("active worktree row count = %d, want 1 (no duplicate row from the adopt call)", got)
	}
}

// TestWorktreeCleanupNeverReapsDetachedAdoptedWorktreeEvenWithIncludeAdopted
// pins the dangerous regression: strings.Contains(x, "") is always true in
// Go, so a naive merged-ness check on a branch-less (Branch == "") adopted
// worktree would read as "merged" against ANY `git branch --merged` output.
// A detached adopted worktree must never be reaped by --merged, even with
// --include-adopted, precisely in the common detached case that flag exists
// to allow reaping in general.
func TestWorktreeCleanupNeverReapsDetachedAdoptedWorktreeEvenWithIncludeAdopted(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	localPath := setupFreshWorktreeRepo(t, home, root, "auto", false)

	head := strings.TrimSpace(runGitOutput(t, localPath, "rev-parse", "HEAD"))
	extWT := filepath.Join(t.TempDir(), "external-wt")
	runGit(t, localPath, "worktree", "add", "--detach", extWT, head)

	if _, stderr, err := executeForTest("--home", home, "--root", root, "worktree", "adopt", extWT); err != nil {
		t.Fatalf("adopt stderr=%q err=%v", stderr, err)
	}

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "worktree", "cleanup", "--merged", "--include-adopted", "--json")
	if err != nil {
		t.Fatalf("cleanup stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	var out worktreeCleanupResult
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("decode cleanup --json: %v\n%s", err, stdout)
	}
	if out.Removed != 0 {
		t.Fatalf("cleanup removed %d worktrees, want 0 (a detached adopted worktree must never be merge-eligible): %+v", out.Removed, out)
	}
	if got := worktreeRowCountForTest(t, filepath.Join(home, "state.db")); got != 1 {
		t.Fatalf("active worktree row count = %d, want 1 (the adopted row must survive)", got)
	}
}

// TestWorktreeCleanupSkipsAdoptedByDefault proves --include-adopted is
// required at all: without it, an adopted worktree is skipped even though
// (in this fixture) it WOULD be merge-eligible if it were a branched,
// `worktree new`-created row.
func TestWorktreeCleanupSkipsAdoptedByDefault(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	localPath := setupFreshWorktreeRepo(t, home, root, "auto", false)

	head := strings.TrimSpace(runGitOutput(t, localPath, "rev-parse", "HEAD"))
	extWT := filepath.Join(t.TempDir(), "external-wt")
	runGit(t, localPath, "worktree", "add", "--detach", extWT, head)
	if _, stderr, err := executeForTest("--home", home, "--root", root, "worktree", "adopt", extWT); err != nil {
		t.Fatalf("adopt stderr=%q err=%v", stderr, err)
	}

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "worktree", "cleanup", "--merged", "--json")
	if err != nil {
		t.Fatalf("cleanup stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	var out worktreeCleanupResult
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("decode cleanup --json: %v\n%s", err, stdout)
	}
	if out.Removed != 0 || out.Skipped != 1 {
		t.Fatalf("cleanup = %+v, want 0 removed / 1 skipped (adopted worktrees are skipped by default)", out)
	}
}

// TestWorktreeRemoveOnAdoptedDeregistersOnlyUnlessPrune proves `worktree
// remove` on an adopted row leaves the physical checkout in place by
// default, and only removes it with --prune.
func TestWorktreeRemoveOnAdoptedDeregistersOnlyUnlessPrune(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	localPath := setupFreshWorktreeRepo(t, home, root, "auto", false)

	head := strings.TrimSpace(runGitOutput(t, localPath, "rev-parse", "HEAD"))
	extWT := filepath.Join(t.TempDir(), "external-wt")
	runGit(t, localPath, "worktree", "add", "--detach", extWT, head)

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "worktree", "adopt", extWT, "--json")
	if err != nil {
		t.Fatalf("adopt stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	var adopted adoptResultForTest
	if err := json.Unmarshal([]byte(stdout), &adopted); err != nil {
		t.Fatal(err)
	}

	if _, stderr, err := executeForTest("--home", home, "--root", root, "worktree", "remove", adopted.ID); err != nil {
		t.Fatalf("remove stderr=%q err=%v", stderr, err)
	}
	if _, statErr := os.Stat(extWT); statErr != nil {
		t.Fatalf("checkout at %s was removed by a plain `worktree remove`; adopted worktrees must deregister only by default (stat err: %v)", extWT, statErr)
	}
	if got := worktreeStatusForTest(t, filepath.Join(home, "state.db"), adopted.ID); got != "removed" {
		t.Fatalf("worktree row status = %q, want removed", got)
	}

	// Re-adopt (the row is 'removed', so the active-path unique index does
	// not block a fresh registration at the same path), then remove --prune
	// to prove the physical checkout DOES go away when explicitly requested.
	if _, stderr, err := executeForTest("--home", home, "--root", root, "worktree", "adopt", extWT); err != nil {
		t.Fatalf("re-adopt stderr=%q err=%v", stderr, err)
	}
	// listWorktreesForTest only returns 'active' rows, and this fixture never
	// creates any other worktree, so the sole survivor is the re-adopted one.
	// (A raw string compare against extWT would spuriously fail here: adopt
	// stores the EvalSymlinks-resolved path, e.g. /private/var/... on macOS,
	// while extWT itself is the unresolved t.TempDir() spelling.)
	worktrees := listWorktreesForTest(t, home, root)
	if len(worktrees) != 1 {
		t.Fatalf("active worktree count = %d, want 1: %+v", len(worktrees), worktrees)
	}
	reAdoptedID := worktrees[0].ID
	if _, stderr, err := executeForTest("--home", home, "--root", root, "worktree", "remove", reAdoptedID, "--prune"); err != nil {
		t.Fatalf("remove --prune stderr=%q err=%v", stderr, err)
	}
	if _, statErr := os.Stat(extWT); statErr == nil {
		t.Fatalf("checkout at %s survived `worktree remove --prune`", extWT)
	}
}

// TestAgentPrRefusesBranchlessAdoptedWorktree pins the item-5(c) gate: a
// detached-HEAD (branch-less) worktree has nothing to push, and PushBranch
// does no branch-name validation, so a raw `git push -u origin ""` would
// otherwise fail bare — and only AFTER BaseDrift's network fetch already ran.
// `agent pr` must refuse up front with an actionable remedy instead.
func TestAgentPrRefusesBranchlessAdoptedWorktree(t *testing.T) {
	ctx := context.Background()
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	localPath := setupFreshWorktreeRepo(t, home, root, "auto", false)

	head := strings.TrimSpace(runGitOutput(t, localPath, "rev-parse", "HEAD"))
	extWT := filepath.Join(t.TempDir(), "external-wt")
	runGit(t, localPath, "worktree", "add", "--detach", extWT, head)

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "worktree", "adopt", extWT, "--json")
	if err != nil {
		t.Fatalf("adopt stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	var adopted adoptResultForTest
	if err := json.Unmarshal([]byte(stdout), &adopted); err != nil {
		t.Fatal(err)
	}
	if adopted.Branch != "" {
		t.Fatalf("fixture invariant broken: adopted.Branch = %q, want empty", adopted.Branch)
	}

	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	project, err := store.ProjectByPath(ctx, "work/acme/repo")
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.InsertAgentRun(ctx, state.AgentRun{
		NamespaceID: project.ID,
		WorktreeID:  adopted.ID,
		Engine:      "generic",
		Task:        "branchless pr refusal",
		Status:      "complete",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, stderr, err = executeForTest("--home", home, "--root", root, "agent", "pr", run.ID, "--dry-run", "--allow-stale-base")
	if err == nil {
		t.Fatal("want a refusal creating a PR for a branchless worktree")
	}
	if !strings.Contains(stderr, "no branch") || !strings.Contains(stderr, "git switch -c") {
		t.Fatalf("stderr = %q, want a no-branch refusal naming the git switch -c remedy", stderr)
	}
}

// TestWorktreeAdoptRecordsAncestorNotBaseTip is the test that pins AD5-02's
// CENTRAL semantic, and it exists because the rest of the suite could not.
//
// Every other adopt fixture creates its worktree at the base ref's current tip,
// so merge-base and tip are the same commit and both implementations agree. A
// mutant that discards the merge-base and records `RevParse(baseRef)` — the raw
// tip, i.e. the exact "every adopted worktree reports fresh forever" bug this
// feature exists to prevent — passed every adopt test AND worktree_adopt.txtar.
// spec/14's AD-5 invariant demands a check that fails when the behavior is
// reverted, so this test advances the base PAST the worktree before adopting and
// asserts the recorded base is the ancestor the work actually sits on.
//
// Mutation-checked: record the tip instead and this fails with tip != ancestor.
func TestWorktreeAdoptRecordsAncestorNotBaseTip(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	localPath := setupFreshWorktreeRepo(t, home, root, "auto", false)

	ancestor := strings.TrimSpace(runGitOutput(t, localPath, "rev-parse", "HEAD"))
	extWT := filepath.Join(t.TempDir(), "external-wt")
	runGit(t, localPath, "worktree", "add", "--detach", extWT, ancestor)

	// Advance the base ref past the adopted worktree, so tip != ancestor and the
	// two candidate implementations finally disagree.
	if err := os.WriteFile(filepath.Join(localPath, "AFTER.md"), []byte("later\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, localPath, "add", "AFTER.md")
	runGit(t, localPath, "commit", "-m", "advance the base past the worktree")
	runGit(t, localPath, "push", "origin", "main")
	runGit(t, localPath, "fetch", "origin")

	tip := strings.TrimSpace(runGitOutput(t, localPath, "rev-parse", "origin/main"))
	if tip == ancestor {
		t.Fatalf("fixture failed to advance the base: tip == ancestor == %s; the test would prove nothing", tip)
	}

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "worktree", "adopt", extWT, "--json")
	if err != nil {
		t.Fatalf("adopt stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	var out adoptResultForTest
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("decode adopt --json: %v\n%s", err, stdout)
	}
	if out.BaseSHA == tip {
		t.Fatalf("BaseSHA recorded the base ref's current TIP (%s). That makes every adopted worktree "+
			"report fresh forever and turns the stale-base gate into a lie — the exact defect AD5-02 exists to prevent.", tip)
	}
	if out.BaseSHA != ancestor {
		t.Fatalf("BaseSHA = %q, want the merge-base ancestor %q (the commit this worktree's work actually sits on)", out.BaseSHA, ancestor)
	}
}

// TestWorktreeAdoptRefusesProjectMismatch pins that --project DISAMBIGUATES
// rather than overrides. Trusting the flag would let a caller register a
// worktree under a namespace whose checkout is a different repository entirely,
// corrupting provenance — and `worktree remove --prune` would then run
// `git worktree remove` from the wrong repo.
func TestWorktreeAdoptRefusesProjectMismatch(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	localPath := setupFreshWorktreeRepo(t, home, root, "auto", false)

	// A second, unrelated adopted project. It must live INSIDE the workspace
	// root or `scan --adopt` refuses it — and a skipped test would prove
	// nothing about the refusal this exercises.
	other := filepath.Join(root, "other-src")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, other, "init")
	runGit(t, other, "config", "user.email", "devstrap@example.test")
	runGit(t, other, "config", "user.name", "DevStrap Test")
	if err := os.WriteFile(filepath.Join(other, "README.md"), []byte("other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, other, "add", "README.md")
	runGit(t, other, "commit", "-m", "other initial")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "scan", root, "--adopt"); err != nil {
		t.Fatalf("adopting the second project failed: %v (%s)", err, stderr)
	}

	head := strings.TrimSpace(runGitOutput(t, localPath, "rev-parse", "HEAD"))
	extWT := filepath.Join(t.TempDir(), "external-wt")
	runGit(t, localPath, "worktree", "add", "--detach", extWT, head)

	// Name a project that is NOT this worktree's repository.
	_, stderr, err := executeForTest("--home", home, "--root", root, "worktree", "adopt", extWT, "--project", "other-src")
	if err == nil {
		t.Fatal("want a refusal when --project names a different repository than the worktree's main checkout")
	}
	if !strings.Contains(stderr, "disambiguates") && !strings.Contains(stderr, "belongs to") {
		t.Fatalf("stderr = %q, want a mismatch refusal explaining --project cannot reassign a worktree", stderr)
	}
}

// TestWorktreeListShowsProvenance pins AD5-06's actual scope. `--json` already
// carried provenance (it encodes state.Worktree, which has always had
// created_by), so the real gap was human output: a user could not tell a
// DevStrap-created worktree from an adopted one, even though the two have
// different reap semantics — cleanup skips adopted rows without
// --include-adopted, and remove deregisters rather than deleting.
func TestWorktreeListShowsProvenance(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	localPath := setupFreshWorktreeRepo(t, home, root, "auto", false)

	head := strings.TrimSpace(runGitOutput(t, localPath, "rev-parse", "HEAD"))
	extWT := filepath.Join(t.TempDir(), "external-wt")
	runGit(t, localPath, "worktree", "add", "--detach", extWT, head)
	if _, stderr, err := executeForTest("--home", home, "--root", root, "worktree", "adopt", extWT); err != nil {
		t.Fatalf("adopt stderr=%q err=%v", stderr, err)
	}

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "worktree", "list")
	if err != nil {
		t.Fatalf("list stderr=%q err=%v", stderr, err)
	}
	if !strings.Contains(stdout, "adopted") {
		t.Errorf("human `worktree list` must show provenance so an adopted worktree is\n"+
			"distinguishable from one DevStrap created; got:\n%s", stdout)
	}
	// A detached adopted worktree stores Branch == "" — the contract --json
	// consumers see. Rendering that as a blank column reads as a bug.
	if !strings.Contains(stdout, "(detached)") {
		t.Errorf("a detached adopted worktree must render a labelled branch column, not a blank one; got:\n%s", stdout)
	}

	// --json must be UNCHANGED: created_by was always there, and adding a
	// derived field would break the additive-only contract for no gain.
	jsonOut, _, err := executeForTest("--home", home, "--root", root, "--json", "worktree", "list")
	if err != nil {
		t.Fatalf("list --json err=%v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(jsonOut), &rows); err != nil {
		t.Fatalf("decode list --json: %v\n%s", err, jsonOut)
	}
	if len(rows) == 0 {
		t.Fatal("expected at least the adopted worktree in --json output")
	}
	if _, ok := rows[0]["created_by"]; !ok {
		t.Errorf("created_by must remain in --json (it is how a machine consumer reads provenance); got keys %v", rows[0])
	}
	if _, ok := rows[0]["adopted"]; ok {
		t.Errorf("--json must NOT gain a derived `adopted` field: created_by already carries this, " +
			"and spec/13 forbids wrapping a store type just to rename what it already says")
	}
}

// TestWorktreeAdoptRefreshesBranchAfterSwitch pins P8-ADOPT-02. A worktree
// adopted while detached records branch="". Before this fix no code path ever
// updated that column, so `agent pr`'s own printed remedy — "create one first
// with 'git switch -c <name>', then re-run" — could never take effect: the user
// followed it and hit the identical refusal forever. A remedy that cannot work
// is worse than no remedy, because it sends the user in a circle.
func TestWorktreeAdoptRefreshesBranchAfterSwitch(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	localPath := setupFreshWorktreeRepo(t, home, root, "auto", false)

	head := strings.TrimSpace(runGitOutput(t, localPath, "rev-parse", "HEAD"))
	extWT := filepath.Join(t.TempDir(), "external-wt")
	runGit(t, localPath, "worktree", "add", "--detach", extWT, head)

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "worktree", "adopt", extWT, "--json")
	if err != nil {
		t.Fatalf("adopt stderr=%q err=%v", stderr, err)
	}
	var first adoptResultForTest
	if err := json.Unmarshal([]byte(stdout), &first); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout)
	}
	if first.Branch != "" {
		t.Fatalf("fixture is wrong: a detached adopt must record an empty branch, got %q", first.Branch)
	}

	// The user follows the remedy `agent pr` prints.
	runGit(t, extWT, "switch", "-c", "feature/fix-login")

	stdout, stderr, err = executeForTest("--home", home, "--root", root, "worktree", "adopt", extWT, "--json")
	if err != nil {
		t.Fatalf("re-adopt stderr=%q err=%v", stderr, err)
	}
	var second adoptResultForTest
	if err := json.Unmarshal([]byte(stdout), &second); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout)
	}
	if !second.AlreadyAdopted {
		t.Errorf("re-adopting an adopted row should report already_adopted; got %+v", second)
	}
	if second.Branch != "feature/fix-login" {
		t.Fatalf("branch = %q, want feature/fix-login — the remedy the CLI prints must actually take effect", second.Branch)
	}

	// Read the row BACK. The assertion above only proves the in-memory struct
	// adopt returned; it passes even if the UPDATE never wrote the column, which
	// a mutation check caught. What matters is that the next command sees the
	// branch, so verify through a separate invocation that re-reads the store.
	listOut, _, err := executeForTest("--home", home, "--root", root, "--json", "worktree", "list")
	if err != nil {
		t.Fatalf("worktree list: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(listOut), &rows); err != nil {
		t.Fatalf("decode list: %v\n%s", err, listOut)
	}
	var persisted string
	for _, r := range rows {
		if p, _ := r["path"].(string); p == second.Path {
			persisted, _ = r["branch"].(string)
		}
	}
	if persisted != "feature/fix-login" {
		t.Fatalf("PERSISTED branch = %q, want feature/fix-login; the adoption UPDATE must write the column, not just the returned struct", persisted)
	}
}

// TestWorktreeAdoptValidatesBaseRefShape pins P8-ADOPT-03's record-time half.
// MergeBase accepts any committish, so an unvalidated --base-ref was stored
// verbatim and only rejected much later by BaseDrift's remote/branch split —
// leaving the worktree adopted but permanently unusable by `worktree status`,
// `finalize`, and `agent pr`, with no remedy named. `--base-ref main` is the
// natural mistake and hit exactly that.
func TestWorktreeAdoptValidatesBaseRefShape(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	localPath := setupFreshWorktreeRepo(t, home, root, "auto", false)

	head := strings.TrimSpace(runGitOutput(t, localPath, "rev-parse", "HEAD"))
	extWT := filepath.Join(t.TempDir(), "external-wt")
	runGit(t, localPath, "worktree", "add", "--detach", extWT, head)

	for _, tc := range []struct {
		name, baseRef, wantMsg string
	}{
		{"bare branch name", "main", "<remote>/<branch>"},
		{"devstrap working-state plane", "refs/devstrap/wip/dev_x/work-proj", "working-state plane"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, err := executeForTest("--home", home, "--root", root,
				"worktree", "adopt", extWT, "--base-ref", tc.baseRef)
			if err == nil {
				t.Fatalf("want a refusal for --base-ref %q", tc.baseRef)
			}
			if !strings.Contains(stderr, tc.wantMsg) {
				t.Fatalf("stderr = %q, want it to explain %q", stderr, tc.wantMsg)
			}
			// And nothing may have been registered.
			listOut, _, lerr := executeForTest("--home", home, "--root", root, "--json", "worktree", "list")
			if lerr != nil {
				t.Fatal(lerr)
			}
			if strings.Contains(listOut, extWT) {
				t.Fatalf("a refused adopt must register nothing; worktree list contains %s", extWT)
			}
		})
	}

	// The qualified form still works — the validation must not refuse the
	// legitimate orphan-branch case the error message elsewhere recommends.
	if _, stderr, err := executeForTest("--home", home, "--root", root,
		"worktree", "adopt", extWT, "--base-ref", "origin/main"); err != nil {
		t.Fatalf("origin/main must be accepted: stderr=%q err=%v", stderr, err)
	}
}
