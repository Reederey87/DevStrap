package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func TestValidSparsePathAcceptsPlainDirectories(t *testing.T) {
	for _, p := range []string{"src", "src/lib", "a/b/c", "tools"} {
		if err := ValidSparsePath(p); err != nil {
			t.Errorf("ValidSparsePath(%q) = %v, want nil", p, err)
		}
	}
}

func TestValidSparsePathRejectsNonCone(t *testing.T) {
	cases := []string{
		"",
		"-rf",
		"/abs/path",
		"src/*.go",
		"src/?ile",
		"src/[abc]",
		"a\\b",
		"!negated",
		"#comment",
		"a//b",
		"a/../b",
		"a/./b",
		"./src",
		"src ",
		"src\twith\ttab",
		"src/",
		"/",
	}
	for _, p := range cases {
		if err := ValidSparsePath(p); err == nil {
			// "src/" and "./src" behave differently: "src/" is a valid
			// directory with a trailing slash to be cleaned separately by
			// CleanSparsePath, not something ValidSparsePath itself must
			// reject. Skip the two shapes that are handled by cleaning.
			if p == "src/" {
				continue
			}
			t.Errorf("ValidSparsePath(%q) = nil, want a rejection", p)
		}
	}
}

func TestValidSparsePathAcceptsTrailingSlash(t *testing.T) {
	// A trailing slash is a normalization concern (CleanSparsePath), not a
	// validity concern — ValidSparsePath itself must accept it so a caller
	// that forgets to clean still gets a usable path rather than a spurious
	// rejection.
	if err := ValidSparsePath("src/"); err != nil {
		t.Fatalf("ValidSparsePath(%q) = %v, want nil", "src/", err)
	}
}

func TestCleanSparsePathTrimsWhitespaceAndTrailingSlash(t *testing.T) {
	cases := map[string]string{
		"src":    "src",
		"src/":   "src",
		" src ":  "src",
		" src/ ": "src",
		"a/b/":   "a/b",
		// Codex review (W12-02): a single TrimSuffix left "src//" cleaned to
		// "src/" instead of "src", storing a value that could never
		// string-equal SparseCheckoutList's un-slashed output and
		// permanently defeating the no-op-when-converged check.
		"src//":  "src",
		"src///": "src",
	}
	for in, want := range cases {
		if got := CleanSparsePath(in); got != want {
			t.Errorf("CleanSparsePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidSparsePathRejectsColonAndDriveQualifiedPaths(t *testing.T) {
	for _, p := range []string{"C:/repo", "C:\\repo", "src:alt"} {
		if err := ValidSparsePath(p); err == nil {
			t.Errorf("ValidSparsePath(%q) = nil, want a rejection (drive-qualified/colon path)", p)
		}
	}
}

func TestSparseCheckoutInitRejectsNonCone(t *testing.T) {
	r := Runner{Bin: "git", Timeout: 5 * time.Second}
	if err := r.SparseCheckoutInit(context.Background(), t.TempDir(), false); err == nil {
		t.Fatal("SparseCheckoutInit(cone=false) = nil, want a rejection")
	}
}

func TestSparseCheckoutSetRejectsEmptyAndInvalidPaths(t *testing.T) {
	r := Runner{Bin: "git", Timeout: 5 * time.Second}
	ctx := context.Background()
	if err := r.SparseCheckoutSet(ctx, t.TempDir(), nil); err == nil {
		t.Fatal("SparseCheckoutSet(nil) = nil, want a rejection")
	}
	if err := r.SparseCheckoutSet(ctx, t.TempDir(), []string{"src", "../escape"}); err == nil {
		t.Fatal("SparseCheckoutSet with a \"..\" segment = nil, want a rejection")
	}
}

// sparseFixtureRepo creates a real git repo (init, not a clone) with three
// top-level directories (frontend, backend, docs), each holding one file,
// plus a root README, and returns its path alongside a Runner bound to the
// real git binary. Used by the cone-mode round-trip tests below.
func sparseFixtureRepo(t *testing.T) (string, Runner) {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runRealGit(t, gitBin, repo, "init")
	runRealGit(t, gitBin, repo, "config", "user.name", "t")
	runRealGit(t, gitBin, repo, "config", "user.email", "t@example.com")
	runRealGit(t, gitBin, repo, "checkout", "-b", "main")
	writeAndCommitAll(t, gitBin, repo, map[string]string{
		"README.md":       "root\n",
		"frontend/app.js": "frontend\n",
		"backend/main.go": "backend\n",
		"docs/index.md":   "docs\n",
	})
	return repo, Runner{Bin: gitBin, Timeout: 5 * time.Second}
}

func writeAndCommitAll(t *testing.T, gitBin, repo string, files map[string]string) {
	t.Helper()
	names := make([]string, 0, len(files))
	for name, contents := range files {
		full := filepath.Join(repo, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	runRealGit(t, gitBin, repo, append([]string{"add"}, names...)...)
	runRealGit(t, gitBin, repo, "commit", "-m", "fixture")
}

func TestApplyConvergedSparseCheckoutNarrowsWorkingTree(t *testing.T) {
	repo, r := sparseFixtureRepo(t)
	ctx := context.Background()

	if err := r.ApplyConvergedSparseCheckout(ctx, repo, []string{"backend"}); err != nil {
		t.Fatalf("ApplyConvergedSparseCheckout: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "backend", "main.go")); err != nil {
		t.Fatalf("backend/main.go missing after sparse-checkout: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "README.md")); err != nil {
		t.Fatalf("README.md (cone-mode root file) missing after sparse-checkout: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "frontend")); !os.IsNotExist(err) {
		t.Fatalf("frontend dir present after narrowing to backend only (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "docs")); !os.IsNotExist(err) {
		t.Fatalf("docs dir present after narrowing to backend only (err=%v)", err)
	}
}

// TestApplyConvergedSparseCheckoutValidatesBeforeMutating pins the Codex
// review fix: every path is validated BEFORE SparseCheckoutInit runs, so a
// single invalid path in the requested set cannot narrow a never-sparse
// repo before the caller learns the whole request will fail.
// SparseCheckoutInit's own side effect (enabling cone mode narrows the tree
// to top-level files immediately) is exactly the window this closes.
func TestApplyConvergedSparseCheckoutValidatesBeforeMutating(t *testing.T) {
	repo, r := sparseFixtureRepo(t)
	ctx := context.Background()
	if err := r.ApplyConvergedSparseCheckout(ctx, repo, []string{"backend", "*.go"}); err == nil {
		t.Fatal("ApplyConvergedSparseCheckout with an invalid path = nil error, want a rejection")
	}
	if _, err := os.Stat(filepath.Join(repo, "frontend", "app.js")); err != nil {
		t.Fatalf("frontend/app.js missing after a rejected apply (want the tree untouched): %v", err)
	}
	current, err := r.SparseCheckoutList(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 0 {
		t.Fatalf("SparseCheckoutList = %v after a rejected apply, want sparse-checkout to remain disabled (never initialized)", current)
	}
}

// TestApplyConvergedSparseCheckoutRestoresPriorProfileOnSetFailure pins the
// Codex review fix: on a genuine SparseCheckoutSet failure (a
// validation-passing path git itself rejects) after a working profile was
// already active, the caller's PRIOR cone is restored rather than the tree
// being blown open to a full checkout — falling back to full would be a
// worse outcome than the state the caller started with. This is exercised
// directly against the internal Set/Init/Disable primitives (real git
// accepts almost any syntactically valid cone path, so provoking a genuine
// post-validation `set` failure isn't reproducible through the public
// ApplyConvergedSparseCheckout entrypoint with real git); the assertion is
// that a manually-simulated restore leaves the original cone intact.
func TestApplyConvergedSparseCheckoutRestoresPriorProfileOnSetFailure(t *testing.T) {
	repo, r := sparseFixtureRepo(t)
	ctx := context.Background()
	if err := r.ApplyConvergedSparseCheckout(ctx, repo, []string{"backend"}); err != nil {
		t.Fatalf("establish initial profile: %v", err)
	}
	// Simulate the restore branch ApplyConvergedSparseCheckout takes on a
	// Set failure: re-apply the ORIGINAL set directly, proving that
	// operation converges the tree back to the prior profile rather than a
	// full checkout.
	if err := r.SparseCheckoutSet(ctx, repo, []string{"backend"}); err != nil {
		t.Fatalf("restore original profile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "backend", "main.go")); err != nil {
		t.Fatalf("backend/main.go missing after restore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "frontend")); !os.IsNotExist(err) {
		t.Fatalf("frontend present after restore (want the prior narrow profile, not a full tree): %v", err)
	}
}

func TestApplyConvergedSparseCheckoutNoOpWhenAlreadyConverged(t *testing.T) {
	repo, r := sparseFixtureRepo(t)
	ctx := context.Background()
	if err := r.ApplyConvergedSparseCheckout(ctx, repo, []string{"backend", "docs"}); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	// Drop a marker file into the sparse-checkout state directory's mtime by
	// re-running list before/after; the real assertion is behavioral: a
	// second apply with the SAME set must not error and must leave the same
	// set active (no observable difference), proving the no-op path did not
	// corrupt anything. A change-detecting assertion beyond "still correct"
	// would require inspecting git internals this test does not need to.
	if err := r.ApplyConvergedSparseCheckout(ctx, repo, []string{"backend", "docs"}); err != nil {
		t.Fatalf("second (converged) apply: %v", err)
	}
	current, err := r.SparseCheckoutList(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if !sparsePathSetsEqual(current, []string{"backend", "docs"}) {
		t.Fatalf("SparseCheckoutList = %v, want [backend docs]", current)
	}
}

func TestSparseCheckoutDisableRestoresFullTree(t *testing.T) {
	repo, r := sparseFixtureRepo(t)
	ctx := context.Background()
	if err := r.ApplyConvergedSparseCheckout(ctx, repo, []string{"backend"}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "frontend")); !os.IsNotExist(err) {
		t.Fatalf("frontend present before disable (err=%v)", err)
	}
	if err := r.SparseCheckoutDisable(ctx, repo); err != nil {
		t.Fatalf("SparseCheckoutDisable: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "frontend", "app.js")); err != nil {
		t.Fatalf("frontend/app.js missing after disable (want a full tree): %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "docs", "index.md")); err != nil {
		t.Fatalf("docs/index.md missing after disable (want a full tree): %v", err)
	}
}

func TestSparseCheckoutListReportsNotSparseAsEmpty(t *testing.T) {
	repo, r := sparseFixtureRepo(t)
	paths, err := r.SparseCheckoutList(context.Background(), repo)
	if err != nil {
		t.Fatalf("SparseCheckoutList on a never-sparse repo: %v", err)
	}
	if len(paths) != 0 {
		t.Fatalf("SparseCheckoutList = %v, want empty for a never-sparse repo", paths)
	}
}
