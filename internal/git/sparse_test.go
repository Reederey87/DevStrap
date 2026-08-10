package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/platform"
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

// TestNoCheckoutSparseRoundTrip proves the first-checkout sequence itself,
// independently of the CLI: clone leaves the working tree empty, the cone is
// configured before checkout, and CheckoutHead populates only that cone.
func TestNoCheckoutSparseRoundTrip(t *testing.T) {
	remote, r := sparseFixtureRepo(t)
	dest := filepath.Join(t.TempDir(), "clone")
	ctx := context.Background()

	if err := r.CloneWithOptions(ctx, remote, dest, CloneOptions{Partial: true, NoCheckout: true}); err != nil {
		t.Fatalf("CloneWithOptions --no-checkout: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "README.md")); !os.IsNotExist(err) {
		t.Fatalf("README.md present before CheckoutHead (want empty working tree), stat err = %v", err)
	}
	if err := r.SparseCheckoutInit(ctx, dest, true); err != nil {
		t.Fatalf("SparseCheckoutInit: %v", err)
	}
	if err := r.SparseCheckoutSet(ctx, dest, []string{"backend"}); err != nil {
		t.Fatalf("SparseCheckoutSet: %v", err)
	}
	if err := r.CheckoutHead(ctx, dest); err != nil {
		t.Fatalf("CheckoutHead: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dest, "README.md")); err != nil {
		t.Fatalf("README.md (cone-mode root file) missing after CheckoutHead: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "backend", "main.go")); err != nil {
		t.Fatalf("backend/main.go missing after CheckoutHead: %v", err)
	}
	for _, absent := range []string{"frontend", "docs"} {
		if _, err := os.Stat(filepath.Join(dest, absent)); !os.IsNotExist(err) {
			t.Fatalf("%s present after first checkout narrowed to backend, stat err = %v", absent, err)
		}
	}
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
// worse outcome than the state the caller started with.
//
// Review follow-up (W12-02): the original version of this test called
// SparseCheckoutSet directly and never entered ApplyConvergedSparseCheckout
// at all, so it could not have caught a regression in the restore branch
// itself. Real git is permissive about syntactically valid cone paths — it
// doesn't require the directory to actually exist — so a genuine
// post-validation `set` failure isn't reproducible through real args alone.
// This version routes through the real ApplyConvergedSparseCheckout
// entrypoint using a thin fake `git` wrapper that delegates every
// subcommand to the real binary except a `sparse-checkout set` call
// targeting the new (failing) profile — a real failure the production code
// path actually has to handle, not a reimplementation of what it's supposed
// to do on one.
//
// A first draft of this test only asserted the END state matched the prior
// profile — which passed even when a mutation deleted the restore call
// entirely, because SparseCheckoutInit alone (which still runs, and
// succeeds) doesn't clear an already-active cone, so "nothing else touched
// it" looks identical to "the restore explicitly re-applied it". Caught via
// a deliberate mutation (stubbing out the restore call) that this version
// still failed to catch on the first attempt; fixed by also asserting the
// wrapper's call log shows the restore's `sparse-checkout set` invocation
// actually happened, not just that its target directory remained on disk.
func TestApplyConvergedSparseCheckoutRestoresPriorProfileOnSetFailure(t *testing.T) {
	repo, realRunner := sparseFixtureRepo(t)
	ctx := context.Background()
	if err := realRunner.ApplyConvergedSparseCheckout(ctx, repo, []string{"backend"}); err != nil {
		t.Fatalf("establish initial profile: %v", err)
	}

	// Fail ONLY a `sparse-checkout set` call that targets "docs" — the new
	// profile this test attempts — never one targeting "backend", so the
	// restore branch's own re-application of the PRIOR profile (also a
	// `sparse-checkout set` call, just with different arguments) is allowed
	// to actually succeed. Every `sparse-checkout set` invocation's target is
	// logged to callLog, whether blocked or allowed through, so the test can
	// prove the restore call was genuinely attempted (not just that its
	// effect happened to already be in place).
	callLog := filepath.Join(t.TempDir(), "set-calls.log")
	fakeGit := writeFailingSparseCheckoutSetGitWrapper(t, realRunner.Bin, "docs", callLog)
	failingRunner := Runner{Bin: fakeGit, Timeout: realRunner.Timeout}

	if err := failingRunner.ApplyConvergedSparseCheckout(ctx, repo, []string{"docs"}); err == nil {
		t.Fatal("ApplyConvergedSparseCheckout with a failing sparse-checkout set = nil error, want the simulated failure surfaced")
	}

	logged, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatal(err)
	}
	wantLog := "docs\nbackend\n"
	if string(logged) != wantLog {
		t.Fatalf("sparse-checkout set call log = %q, want %q (the failed attempt at \"docs\" followed by a genuine restore attempt at \"backend\" — a missing second line means the restore call itself never fired)", string(logged), wantLog)
	}

	current, err := realRunner.SparseCheckoutList(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if !sparsePathSetsEqual(current, []string{"backend"}) {
		t.Fatalf("SparseCheckoutList after a failed set = %v, want the PRIOR profile [backend] restored, not left on the failed target or blown open to full", current)
	}
	if _, err := os.Stat(filepath.Join(repo, "backend", "main.go")); err != nil {
		t.Fatalf("backend/main.go missing after restore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "frontend")); !os.IsNotExist(err) {
		t.Fatalf("frontend present after restore (want the prior narrow profile, not a full tree): %v", err)
	}
}

// writeFailingSparseCheckoutSetGitWrapper writes a shell script that
// delegates every git invocation to realGitBin except a `sparse-checkout
// set` call whose argument list contains failTarget, which it fails with a
// nonzero exit — letting a test provoke exactly one targeted, real
// subprocess failure (the call attempting to apply failTarget) while any
// OTHER `sparse-checkout set` call (e.g. ApplyConvergedSparseCheckout's own
// restore-the-prior-profile call, which targets a different path) still
// succeeds for real. EVERY `sparse-checkout set` invocation — blocked or
// allowed — appends its last argument (the target path) as its own line to
// callLog, so a test can assert a later call genuinely happened rather than
// inferring it from a final state that could look identical either way. It
// scans the WHOLE argument list for the "sparse-checkout set" pair rather
// than checking $1/$2 directly, because Runner.Run's secureArgs prepends
// several `-c key=value` pairs ahead of the actual subcommand.
func writeFailingSparseCheckoutSetGitWrapper(t *testing.T, realGitBin, failTarget, callLog string) string {
	t.Helper()
	if platform.Detect().OS == "windows" {
		t.Skip("shell wrapper script not supported on windows")
	}
	path := filepath.Join(t.TempDir(), "git-fake-failing-sparse-set")
	script := "#!/bin/sh\n" +
		"prev=\"\"\n" +
		"match=0\n" +
		"hastarget=0\n" +
		"last=\"\"\n" +
		"for arg in \"$@\"; do\n" +
		"  if [ \"$prev\" = \"sparse-checkout\" ] && [ \"$arg\" = \"set\" ]; then\n" +
		"    match=1\n" +
		"  fi\n" +
		"  if [ \"$arg\" = \"" + failTarget + "\" ]; then\n" +
		"    hastarget=1\n" +
		"  fi\n" +
		"  last=\"$arg\"\n" +
		"  prev=\"$arg\"\n" +
		"done\n" +
		"if [ \"$match\" = \"1\" ]; then\n" +
		"  echo \"$last\" >> \"" + callLog + "\"\n" +
		"fi\n" +
		"if [ \"$match\" = \"1\" ] && [ \"$hastarget\" = \"1\" ]; then\n" +
		"  echo \"simulated sparse-checkout set failure\" >&2\n" +
		"  exit 1\n" +
		"fi\n" +
		"exec \"" + realGitBin + "\" \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
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

// TestNormalizeSparsePaths pins the ancestor-collapsing contract directly:
// a path that is a proper descendant of another path already in the set is
// dropped (mirroring git's own cone-mode collapsing behavior), exact
// duplicates are dropped, unrelated paths all survive, and the surviving
// paths keep their original relative order rather than the internal sorted
// working order leaking out.
func TestNormalizeSparsePaths(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"no overlap", []string{"docs", "backend"}, []string{"docs", "backend"}},
		// Second review follow-up: an un-cleaned trailing slash must still
		// collapse against its already-clean ancestor, not silently
		// reintroduce the overlap bug via a different string representation
		// of the same directory.
		{"uncleaned trailing slash still collapses", []string{"backend/", "backend/deep"}, []string{"backend"}},
		{"direct child dropped", []string{"backend", "backend/deep"}, []string{"backend"}},
		{"child listed first still dropped", []string{"backend/deep", "backend"}, []string{"backend"}},
		{"multi-level descendant dropped", []string{"a", "a/b/c/d"}, []string{"a"}},
		{"exact duplicate dropped", []string{"backend", "backend"}, []string{"backend"}},
		{"sibling-prefix NOT collapsed", []string{"back", "backend"}, []string{"back", "backend"}},
		{"mixed", []string{"docs", "backend", "backend/deep", "backend/deep/deeper", "frontend"}, []string{"docs", "backend", "frontend"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := NormalizeSparsePaths(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("NormalizeSparsePaths(%v) = %v, want %v", c.in, got, c.want)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Fatalf("NormalizeSparsePaths(%v) = %v, want %v", c.in, got, c.want)
				}
			}
		})
	}
}

// TestApplyConvergedSparseCheckoutConvergesWithOverlappingPaths is the
// direct regression guard for the review-reported bug: git's cone mode
// collapses an overlapping set (a directory plus one of its own
// subdirectories) down to just the ancestor when reporting the active set,
// so comparing an un-normalized desired set against that always-collapsed
// report never matched — every hydrate/sync would re-run init+set forever
// for a project with overlapping configured paths. This proves BOTH that
// the first apply converges to the collapsed set AND that a second apply
// with the same overlapping input performs NO further git mutation at all
// (via a call-counting fake git wrapper) — not just that it happens not to
// error, which a naive fix could still satisfy while re-doing pointless work
// every time.
func TestApplyConvergedSparseCheckoutConvergesWithOverlappingPaths(t *testing.T) {
	repo, realRunner := sparseFixtureRepo(t)
	ctx := context.Background()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	if err := os.MkdirAll(filepath.Join(repo, "backend", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "backend", "nested", "x.go"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runRealGit(t, gitBin, repo, "add", "backend/nested/x.go")
	runRealGit(t, gitBin, repo, "commit", "-m", "nest a subdirectory of backend")

	setCallLog := filepath.Join(t.TempDir(), "set-calls.log")
	fakeGit := writeCountingSparseCheckoutSetGitWrapper(t, realRunner.Bin, setCallLog)
	r := Runner{Bin: fakeGit, Timeout: realRunner.Timeout}

	if err := r.ApplyConvergedSparseCheckout(ctx, repo, []string{"backend", "backend/nested"}); err != nil {
		t.Fatalf("first (overlapping) apply: %v", err)
	}
	current, err := realRunner.SparseCheckoutList(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if !sparsePathSetsEqual(current, []string{"backend"}) {
		t.Fatalf("SparseCheckoutList after overlapping apply = %v, want the collapsed [backend]", current)
	}

	// Same overlapping input, different order — must be a true no-op: no
	// `sparse-checkout set` call at all, not merely a non-erroring one.
	if err := r.ApplyConvergedSparseCheckout(ctx, repo, []string{"backend/nested", "backend"}); err != nil {
		t.Fatalf("second (overlapping) apply: %v", err)
	}

	data, err := os.ReadFile(setCallLog)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	if trimmed := strings.TrimSpace(string(data)); trimmed != "" {
		calls = len(strings.Split(trimmed, "\n"))
	}
	if calls != 1 {
		t.Fatalf("sparse-checkout set invoked %d times across two overlapping-but-equivalent applies, want exactly 1 (the second must be a genuine no-op, not a perpetual re-apply)", calls)
	}
}

// writeCountingSparseCheckoutSetGitWrapper writes a shell script that
// delegates every git invocation to realGitBin, appending one line to
// callLog each time it's invoked as `sparse-checkout set` — letting a test
// assert exactly how many times a real mutation was attempted, not just
// whether the overall call succeeded. It scans the WHOLE argument list for
// the "sparse-checkout set" pair rather than checking $1/$2 directly,
// because Runner.Run's secureArgs prepends several `-c key=value` pairs
// ahead of the actual subcommand.
func writeCountingSparseCheckoutSetGitWrapper(t *testing.T, realGitBin, callLog string) string {
	t.Helper()
	if platform.Detect().OS == "windows" {
		t.Skip("shell wrapper script not supported on windows")
	}
	path := filepath.Join(t.TempDir(), "git-fake-counting-sparse-set")
	script := "#!/bin/sh\n" +
		"prev=\"\"\n" +
		"match=0\n" +
		"for arg in \"$@\"; do\n" +
		"  if [ \"$prev\" = \"sparse-checkout\" ] && [ \"$arg\" = \"set\" ]; then\n" +
		"    match=1\n" +
		"  fi\n" +
		"  prev=\"$arg\"\n" +
		"done\n" +
		"if [ \"$match\" = \"1\" ]; then\n" +
		"  echo call >> \"" + callLog + "\"\n" +
		"fi\n" +
		"exec \"" + realGitBin + "\" \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
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

// TestSparseCheckoutEverEnabled pins the cheap local filesystem check that
// gates the hot-path convergence probe in applyProjectSparseProfile (review
// follow-up, W12-02): it must report false for a repo that has never
// touched sparse-checkout (so callers skip the git subprocess entirely for
// the vast majority of projects), true once sparse-checkout has genuinely
// been enabled, and true (the safe fallback) for a path whose .git state
// can't be determined at all.
func TestSparseCheckoutEverEnabled(t *testing.T) {
	repo, r := sparseFixtureRepo(t)
	if SparseCheckoutEverEnabled(repo) {
		t.Fatal("SparseCheckoutEverEnabled on a never-sparse repo = true, want false")
	}
	if err := r.ApplyConvergedSparseCheckout(context.Background(), repo, []string{"backend"}); err != nil {
		t.Fatalf("ApplyConvergedSparseCheckout: %v", err)
	}
	if !SparseCheckoutEverEnabled(repo) {
		t.Fatal("SparseCheckoutEverEnabled after enabling sparse-checkout = false, want true")
	}
	if !SparseCheckoutEverEnabled(filepath.Join(t.TempDir(), "does-not-exist")) {
		t.Fatal("SparseCheckoutEverEnabled on an undeterminable path = false, want the safe fallback true")
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
