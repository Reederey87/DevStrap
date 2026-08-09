package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/ignore"
	"github.com/Reederey87/DevStrap/internal/state"
)

// createSparseFixtureRemote builds a bare remote seeded with a root README
// plus three top-level directories (frontend, backend, docs), each holding
// one file — a synthetic multi-directory monorepo fixture for the
// sparse-checkout materialization tests below (W12-02).
func createSparseFixtureRemote(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "repo.git")
	seed := filepath.Join(tmp, "seed")
	runGit(t, tmp, "init", "--bare", remote)
	runGit(t, seed, "init")
	runGit(t, seed, "config", "user.email", "devstrap@example.test")
	runGit(t, seed, "config", "user.name", "DevStrap Test")
	runGit(t, seed, "checkout", "-b", "main")
	files := map[string]string{
		"README.md":       "root\n",
		"frontend/app.js": "frontend\n",
		"backend/main.go": "backend\n",
		"docs/index.md":   "docs\n",
	}
	var names []string
	for name, contents := range files {
		full := filepath.Join(seed, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	runGit(t, seed, append([]string{"add"}, names...)...)
	runGit(t, seed, "commit", "-m", "initial")
	runGit(t, seed, "remote", "add", "origin", remote)
	runGit(t, seed, "push", "origin", "main")
	runGit(t, tmp, "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/main")
	return remote
}

func assertDirPresent(t *testing.T, root, rel string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
		t.Fatalf("%s missing (want present): %v", rel, err)
	}
}

func assertDirAbsent(t *testing.T, root, rel string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); !os.IsNotExist(err) {
		t.Fatalf("%s present (want absent from a narrowed cone), stat err = %v", rel, err)
	}
}

// traceGitCommands installs a transparent PATH shim after fixture setup and
// returns its command log. Each line is "<cwd>\t<argv>" and delegates to the
// real git binary, so assertions can distinguish clone-time behavior from an
// equivalent-looking tree narrowed only after clone.
func traceGitCommands(t *testing.T) string {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "git-commands.log")
	shim := filepath.Join(dir, "git")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\t' "$PWD" >> %q
printf '%%s ' "$@" >> %q
printf '\n' >> %q
exec %q "$@"
`, logPath, logPath, logPath, realGit)
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func readGitTrace(t *testing.T, logPath string) []string {
	t.Helper()
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read git command trace: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func gitTraceIndex(t *testing.T, lines []string, needle string) int {
	t.Helper()
	for i, line := range lines {
		if strings.Contains(line, needle) {
			return i
		}
	}
	t.Fatalf("git command trace has no %q command:\n%s", needle, strings.Join(lines, "\n"))
	return -1
}

func failGitCommandContaining(t *testing.T, fragment string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	shim := filepath.Join(dir, "git")
	script := fmt.Sprintf(`#!/bin/sh
case "$*" in
  *%q*) exit 97 ;;
esac
exec %q "$@"
`, fragment, realGit)
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestAddSparseFlagAppliesOnMaterialize proves the full documented flow: `add
// --sparse` persists the profile at adopt time, and the first `hydrate` after
// that materializes ONLY the configured directories — never a full checkout
// that happens to get narrowed later.
func TestAddSparseFlagAppliesOnMaterialize(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	remote := createSparseFixtureRemote(t)

	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "add", "file://"+remote, "--path", "work/acme/mono", "--default-branch", "main", "--sparse", "backend, docs"); err != nil {
		t.Fatalf("add stderr = %q err = %v", stderr, err)
	}
	tracePath := traceGitCommands(t)
	if _, stderr, err := executeForTest("--home", home, "--root", root, "hydrate", "work/acme/mono"); err != nil {
		t.Fatalf("hydrate stderr = %q err = %v", stderr, err)
	}

	local := filepath.Join(root, "work", "acme", "mono")
	assertDirPresent(t, local, "README.md")
	assertDirPresent(t, local, "backend/main.go")
	assertDirPresent(t, local, "docs/index.md")
	assertDirAbsent(t, local, "frontend")

	trace := readGitTrace(t, tracePath)
	cloneAt := gitTraceIndex(t, trace, " clone ")
	initAt := gitTraceIndex(t, trace, " sparse-checkout init --cone ")
	setAt := gitTraceIndex(t, trace, " sparse-checkout set -- backend docs ")
	checkoutAt := gitTraceIndex(t, trace, " checkout ")
	submoduleAt := gitTraceIndex(t, trace, " submodule update --init --recursive ")
	if !strings.Contains(trace[cloneAt], " --no-checkout ") {
		t.Fatalf("sparse clone argv lacks --no-checkout: %s", trace[cloneAt])
	}
	// Assert the sequence is strictly increasing, naming the first pair that
	// is out of order — a single negated conjunction reports only "wrong"
	// and leaves the reader to diff five integers by hand.
	ordered := []struct {
		name string
		at   int
	}{
		{"clone", cloneAt},
		{"sparse-checkout init", initAt},
		{"sparse-checkout set", setAt},
		{"checkout", checkoutAt},
		{"submodule update", submoduleAt},
	}
	for i := 1; i < len(ordered); i++ {
		if ordered[i-1].at >= ordered[i].at {
			t.Fatalf("first-checkout command order is wrong: %q (at %d) must precede %q (at %d)\n%s",
				ordered[i-1].name, ordered[i-1].at, ordered[i].name, ordered[i].at, strings.Join(trace, "\n"))
		}
	}
	for _, at := range []int{initAt, setAt, checkoutAt, submoduleAt} {
		cwd := strings.SplitN(trace[at], "\t", 2)[0]
		if !strings.Contains(filepath.Base(cwd), ignore.StagingDirMarker) {
			t.Fatalf("pre-promotion command ran outside staging: %s", trace[at])
		}
	}
}

// TestHydrateWithoutSparseProfileKeepsCloneArgv pins the zero-cost majority
// path: a project without a configured profile receives the same clone argv,
// without --no-checkout.
func TestHydrateWithoutSparseProfileKeepsCloneArgv(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	remote := createSparseFixtureRemote(t)

	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "add", "file://"+remote, "--path", "work/acme/mono", "--default-branch", "main"); err != nil {
		t.Fatalf("add stderr = %q err = %v", stderr, err)
	}
	tracePath := traceGitCommands(t)
	if _, stderr, err := executeForTest("--home", home, "--root", root, "hydrate", "work/acme/mono"); err != nil {
		t.Fatalf("hydrate stderr = %q err = %v", stderr, err)
	}
	trace := readGitTrace(t, tracePath)
	clone := trace[gitTraceIndex(t, trace, " clone ")]
	if strings.Contains(clone, "--no-checkout") {
		t.Fatalf("no-profile clone argv unexpectedly contains --no-checkout: %s", clone)
	}
}

// TestSparseFirstCloneFallsBackToFullCheckout pins sparse narrowing's
// best-effort contract for both mutation points. Either failure must still
// promote a usable full tree; only CheckoutHead itself may fail hydrate.
func TestSparseFirstCloneFallsBackToFullCheckout(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fragment string
	}{
		{name: "init failure", fragment: "sparse-checkout init --cone"},
		{name: "set failure", fragment: "sparse-checkout set -- backend"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), ".devstrap")
			root := filepath.Join(t.TempDir(), "Code")
			remote := createSparseFixtureRemote(t)

			if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
				t.Fatalf("init stderr = %q err = %v", stderr, err)
			}
			if _, stderr, err := executeForTest("--home", home, "--root", root, "add", "file://"+remote, "--path", "work/acme/mono", "--default-branch", "main", "--sparse", "backend"); err != nil {
				t.Fatalf("add stderr = %q err = %v", stderr, err)
			}
			failGitCommandContaining(t, tc.fragment)
			if _, stderr, err := executeForTest("--home", home, "--root", root, "hydrate", "work/acme/mono"); err != nil {
				t.Fatalf("hydrate must degrade to a full checkout: stderr = %q err = %v", stderr, err)
			}
			local := filepath.Join(root, "work", "acme", "mono")
			assertDirPresent(t, local, "frontend/app.js")
			assertDirPresent(t, local, "backend/main.go")
			assertDirPresent(t, local, "docs/index.md")
		})
	}
}

// TestSparseFirstClonePopulatesSubmodule is the GIT-06 regression trap: Git
// exits zero but leaves submodule directories empty after a no-checkout clone
// plus checkout unless an explicit recursive update follows CheckoutHead.
func TestSparseFirstClonePopulatesSubmodule(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	tmp := t.TempDir()

	sub := filepath.Join(tmp, "sub")
	runGit(t, sub, "init")
	runGit(t, sub, "config", "user.email", "devstrap@example.test")
	runGit(t, sub, "config", "user.name", "DevStrap Test")
	if err := os.WriteFile(filepath.Join(sub, "README.md"), []byte("submodule\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, sub, "add", "README.md")
	runGit(t, sub, "commit", "-m", "submodule fixture")

	remote := filepath.Join(tmp, "super.git")
	seed := filepath.Join(tmp, "super-seed")
	runGit(t, tmp, "init", "--bare", remote)
	runGit(t, seed, "init")
	runGit(t, seed, "config", "user.email", "devstrap@example.test")
	runGit(t, seed, "config", "user.name", "DevStrap Test")
	runGit(t, seed, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("superproject\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "add", "README.md")
	runGit(t, seed, "-c", "protocol.file.allow=always", "submodule", "add", "file://"+sub, "vendor/sub")
	runGit(t, seed, "commit", "-m", "superproject fixture")
	runGit(t, seed, "remote", "add", "origin", remote)
	runGit(t, seed, "push", "origin", "main")
	runGit(t, tmp, "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/main")

	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "add", "file://"+remote, "--path", "work/acme/super", "--default-branch", "main", "--sparse", "vendor"); err != nil {
		t.Fatalf("add stderr = %q err = %v", stderr, err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "hydrate", "work/acme/super"); err != nil {
		t.Fatalf("hydrate stderr = %q err = %v", stderr, err)
	}
	assertDirPresent(t, filepath.Join(root, "work", "acme", "super"), "vendor/sub/README.md")
}

// TestHydrateConvergesAfterProfileConfiguredLater proves the OTHER documented
// path: a project materialized FIRST with a full checkout, then narrowed by
// `project sparse set`, converges to the narrower tree on that command alone
// (no second hydrate/sync needed) because `set` re-applies immediately
// against an already-materialized checkout.
func TestHydrateConvergesAfterProfileConfiguredLater(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	remote := createSparseFixtureRemote(t)

	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "add", "file://"+remote, "--path", "work/acme/mono", "--default-branch", "main"); err != nil {
		t.Fatalf("add stderr = %q err = %v", stderr, err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "hydrate", "work/acme/mono"); err != nil {
		t.Fatalf("hydrate stderr = %q err = %v", stderr, err)
	}
	local := filepath.Join(root, "work", "acme", "mono")
	// Full checkout before any profile is configured.
	assertDirPresent(t, local, "frontend/app.js")
	assertDirPresent(t, local, "backend/main.go")
	assertDirPresent(t, local, "docs/index.md")

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "project", "sparse", "set", "work/acme/mono", "frontend")
	if err != nil {
		t.Fatalf("project sparse set stderr = %q err = %v", stderr, err)
	}
	if !strings.Contains(stdout, "applied to the materialized checkout") {
		t.Fatalf("project sparse set stdout = %q, want immediate-apply confirmation", stdout)
	}

	assertDirPresent(t, local, "frontend/app.js")
	assertDirAbsent(t, local, "backend")
	assertDirAbsent(t, local, "docs")

	// A second hydrate call must converge with no further change (idempotent
	// no-op path — ApplyConvergedSparseCheckout skips an already-matching set).
	if _, stderr, err := executeForTest("--home", home, "--root", root, "hydrate", "work/acme/mono"); err != nil {
		t.Fatalf("second hydrate stderr = %q err = %v", stderr, err)
	}
	assertDirPresent(t, local, "frontend/app.js")
	assertDirAbsent(t, local, "backend")
}

// TestHydrateConvergesToFullWhenDBClearedButTreeStillNarrowed pins the
// bidirectional-convergence fix (Codex review, W12-02): if a project's DB
// profile is empty but the on-disk tree is still narrowed — exactly the
// state `project sparse clear` can leave behind if its own immediate
// SparseCheckoutDisable call had failed — a later hydrate/sync call must
// self-heal it back to full rather than treating "no configured paths" as
// nothing to do. This bypasses the CLI clear command (which would itself
// disable the on-disk cone) to isolate the self-heal path in hydrate.go.
func TestHydrateConvergesToFullWhenDBClearedButTreeStillNarrowed(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	remote := createSparseFixtureRemote(t)

	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "add", "file://"+remote, "--path", "work/acme/mono", "--default-branch", "main", "--sparse", "backend"); err != nil {
		t.Fatalf("add stderr = %q err = %v", stderr, err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "hydrate", "work/acme/mono"); err != nil {
		t.Fatalf("hydrate stderr = %q err = %v", stderr, err)
	}
	local := filepath.Join(root, "work", "acme", "mono")
	assertDirAbsent(t, local, "frontend")

	opts := testOptions(home, root)
	store, err := opts.openState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.ProjectByPath(context.Background(), "work/acme/mono")
	if err != nil {
		closeStore(store)
		t.Fatal(err)
	}
	if err := store.ReplaceSparsePathsForProject(context.Background(), project.ID, nil); err != nil {
		closeStore(store)
		t.Fatal(err)
	}
	closeStore(store)

	// DB says no profile; the on-disk tree still disagrees (still narrowed).
	assertDirAbsent(t, local, "frontend")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "hydrate", "work/acme/mono"); err != nil {
		t.Fatalf("hydrate stderr = %q err = %v", stderr, err)
	}
	assertDirPresent(t, local, "frontend/app.js")
	assertDirPresent(t, local, "docs/index.md")
}

// TestProjectSparseClearRestoresFullTree proves the "clear" direction of
// convergence also works end to end.
func TestProjectSparseClearRestoresFullTree(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	remote := createSparseFixtureRemote(t)

	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "add", "file://"+remote, "--path", "work/acme/mono", "--default-branch", "main", "--sparse", "backend"); err != nil {
		t.Fatalf("add stderr = %q err = %v", stderr, err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "hydrate", "work/acme/mono"); err != nil {
		t.Fatalf("hydrate stderr = %q err = %v", stderr, err)
	}
	local := filepath.Join(root, "work", "acme", "mono")
	assertDirAbsent(t, local, "frontend")

	if _, stderr, err := executeForTest("--home", home, "--root", root, "project", "sparse", "clear", "work/acme/mono"); err != nil {
		t.Fatalf("project sparse clear stderr = %q err = %v", stderr, err)
	}
	assertDirPresent(t, local, "frontend/app.js")
	assertDirPresent(t, local, "docs/index.md")
}

// TestProjectSparseListRendersConfiguredPaths exercises the read-only half of
// the command group, including --json.
func TestProjectSparseListRendersConfiguredPaths(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	remote := createSparseFixtureRemote(t)

	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "add", "file://"+remote, "--path", "work/acme/mono", "--default-branch", "main", "--sparse", "backend,docs"); err != nil {
		t.Fatalf("add stderr = %q err = %v", stderr, err)
	}

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "project", "sparse", "list", "work/acme/mono", "--json")
	if err != nil {
		t.Fatalf("project sparse list stderr = %q err = %v", stderr, err)
	}
	var out struct {
		Path  string   `json:"path"`
		Paths []string `json:"paths"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("unmarshal --json output %q: %v", stdout, err)
	}
	if len(out.Paths) != 2 || out.Paths[0] != "backend" || out.Paths[1] != "docs" {
		t.Fatalf("paths = %v, want [backend docs]", out.Paths)
	}
}

// TestProjectSparseSetRejectsNonConePattern proves the hard cone-mode-only
// requirement is enforced at the CLI boundary, not only deep in the git
// primitive.
func TestProjectSparseSetRejectsNonConePattern(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	remote := createSparseFixtureRemote(t)

	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "add", "file://"+remote, "--path", "work/acme/mono", "--default-branch", "main"); err != nil {
		t.Fatalf("add stderr = %q err = %v", stderr, err)
	}
	if _, _, err := executeForTest("--home", home, "--root", root, "project", "sparse", "set", "work/acme/mono", "*.go"); err == nil {
		t.Fatal("project sparse set with a non-cone glob pattern = nil error, want a rejection")
	}
}

// TestWorktreeNewInheritsProjectSparseProfile is the step the task calls out
// as most likely to be silently missed: a fresh worktree devstrap creates for
// a sparse-configured project must inherit the SAME cone, or the whole
// feature is defeated for agent worktrees on a large monorepo.
func TestWorktreeNewInheritsProjectSparseProfile(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")

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
	files := map[string]string{
		"README.md":       "root\n",
		"frontend/app.js": "frontend\n",
		"backend/main.go": "backend\n",
	}
	var names []string
	for name, contents := range files {
		full := filepath.Join(seed, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	runGit(t, seed, append([]string{"add"}, names...)...)
	runGit(t, seed, "commit", "-m", "initial")
	runGit(t, seed, "remote", "add", "origin", remote)
	runGit(t, seed, "push", "origin", "main")
	runGit(t, tmp, "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/main")

	if _, stderr, err := executeForTest("--home", home, "--root", root, "add", "file://"+remote, "--path", "work/acme/mono", "--default-branch", "main", "--sparse", "backend"); err != nil {
		t.Fatalf("add stderr = %q err = %v", stderr, err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "hydrate", "work/acme/mono"); err != nil {
		t.Fatalf("hydrate stderr = %q err = %v", stderr, err)
	}
	// The primary checkout has its identity configured so worktree creation's
	// own operations against it (fetch etc.) work; committing inside the
	// WORKTREE is not exercised by this test.
	primary := filepath.Join(root, "work", "acme", "mono")
	runGit(t, primary, "config", "user.email", "devstrap@example.test")
	runGit(t, primary, "config", "user.name", "DevStrap Test")

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "worktree", "new", "work/acme/mono", "--fresh-upstream", "--name", "narrow test", "--json")
	if err != nil {
		t.Fatalf("worktree new stderr = %q err = %v", stderr, err)
	}
	var wt struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(stdout), &wt); err != nil {
		t.Fatalf("unmarshal worktree new --json %q: %v", stdout, err)
	}
	if wt.Path == "" {
		t.Fatalf("worktree new --json produced no path: %q", stdout)
	}

	assertDirPresent(t, wt.Path, "README.md")
	assertDirPresent(t, wt.Path, "backend/main.go")
	assertDirAbsent(t, wt.Path, "frontend")
}

// TestWorktreeAdoptWarnsAboutUnappliedSparseProfile pins the deliberate
// scope decision: `worktree adopt` never mutates a checkout it did not
// create, so a configured sparse profile is surfaced as a warning, not
// silently applied nor silently ignored.
func TestWorktreeAdoptWarnsAboutUnappliedSparseProfile(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	remote := createSparseFixtureRemote(t)

	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "add", "file://"+remote, "--path", "work/acme/mono", "--default-branch", "main", "--sparse", "backend"); err != nil {
		t.Fatalf("add stderr = %q err = %v", stderr, err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "hydrate", "work/acme/mono"); err != nil {
		t.Fatalf("hydrate stderr = %q err = %v", stderr, err)
	}
	primary := filepath.Join(root, "work", "acme", "mono")
	runGit(t, primary, "config", "user.email", "devstrap@example.test")
	runGit(t, primary, "config", "user.name", "DevStrap Test")

	// Simulate an externally-created worktree (Codex/Cursor/Devin) the way
	// worktree_test.go's own adopt tests do: a plain `git worktree add`
	// devstrap never touched.
	external := filepath.Join(t.TempDir(), "external-wt")
	runGit(t, primary, "worktree", "add", "--detach", external, "main")

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "worktree", "adopt", external, "--json")
	if err != nil {
		t.Fatalf("worktree adopt stderr = %q err = %v", stderr, err)
	}
	var out struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("unmarshal worktree adopt --json %q: %v", stdout, err)
	}
	found := false
	for _, w := range out.Warnings {
		if strings.Contains(w, "sparse-checkout profile") {
			found = true
		}
	}
	if !found {
		t.Fatalf("worktree adopt warnings = %v, want a sparse-checkout-profile-not-applied warning", out.Warnings)
	}
	// Note: this test does not assert the adopted checkout's tree shape.
	// git's own sparse-checkout state can be inherited by a brand-new linked
	// worktree from the shared repo config depending on git version/config
	// (empirically observed: a raw `git worktree add` here picked up the
	// primary checkout's cone even though nothing in this test enabled
	// per-worktree config) — that is a property of git itself, not of
	// `worktree adopt`. The behavior actually under test is that DevStrap's
	// adopt path issues no sparse-checkout command of its own and instead
	// warns, which the warnings assertion above already pins.
}

// TestParseSparseFlagValidatesAndDedupes exercises the --sparse flag parser
// directly (unit-level, cheaper than a full CLI round trip for edge cases).
func TestParseSparseFlagValidatesAndDedupes(t *testing.T) {
	paths, err := parseSparseFlag(" backend, docs ,backend/")
	if err != nil {
		t.Fatalf("parseSparseFlag: %v", err)
	}
	if len(paths) != 2 || paths[0] != "backend" || paths[1] != "docs" {
		t.Fatalf("paths = %v, want [backend docs] deduplicated (backend and backend/ are the same directory)", paths)
	}

	if _, err := parseSparseFlag("src,*.go"); err == nil {
		t.Fatal("parseSparseFlag with a glob pattern = nil error, want a rejection")
	}

	paths, err = parseSparseFlag("")
	if err != nil {
		t.Fatal(err)
	}
	if paths != nil {
		t.Fatalf("parseSparseFlag(\"\") = %v, want nil", paths)
	}
}

// TestParseSparseFlagNormalizesOverlappingPaths pins the review follow-up
// fix directly at the CLI layer: an ancestor/descendant pair like
// "backend,backend/deep" must be collapsed to just ["backend"] before it's
// ever stored, mirroring what git's own cone mode would report — storing
// the un-collapsed pair would permanently defeat
// ApplyConvergedSparseCheckout's no-op check for this project on every
// future sync/hydrate.
func TestParseSparseFlagNormalizesOverlappingPaths(t *testing.T) {
	paths, err := parseSparseFlag("backend,backend/deep,docs")
	if err != nil {
		t.Fatalf("parseSparseFlag: %v", err)
	}
	if len(paths) != 2 || paths[0] != "backend" || paths[1] != "docs" {
		t.Fatalf("paths = %v, want [backend docs] (backend/deep collapsed into its ancestor backend)", paths)
	}
}

// TestCleanSparseArgsNormalizesOverlappingPaths mirrors
// TestParseSparseFlagNormalizesOverlappingPaths for `project sparse set`'s
// positional-argument parser (review follow-up, W12-02): the two parsers
// share the same normalization requirement and must not regress
// independently.
func TestCleanSparseArgsNormalizesOverlappingPaths(t *testing.T) {
	paths, err := cleanSparseArgs([]string{"backend", "backend/deep", "docs"})
	if err != nil {
		t.Fatalf("cleanSparseArgs: %v", err)
	}
	if len(paths) != 2 || paths[0] != "backend" || paths[1] != "docs" {
		t.Fatalf("paths = %v, want [backend docs] (backend/deep collapsed into its ancestor backend)", paths)
	}
}

// TestApplyProjectSparseProfileBestEffortOnLookupFailure exercises the
// warn-not-fail contract directly: a project with no configured profile is a
// pure no-op (no store/runner interaction beyond the lookup).
func TestApplyProjectSparseProfileNoopWithoutProfile(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	opts := testOptions(home, root)
	store, err := opts.openState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	project := state.ProjectStatus{NamespaceEntry: state.NamespaceEntry{ID: "prj_does_not_exist"}}
	// Must not panic or error the caller — SparsePathsForProject on an unknown
	// ID simply returns an empty slice (no rows), which is applyProjectSparseProfile's
	// legitimate "no profile configured" no-op path.
	applyProjectSparseProfile(context.Background(), store, gitRunner(opts), project, t.TempDir())
}

// failGitCommandsContaining is failGitCommandContaining for more than one
// fragment, needed to simulate a sparse failure whose rollback ALSO fails.
func failGitCommandsContaining(t *testing.T, fragments ...string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	shim := filepath.Join(dir, "git")
	var cases strings.Builder
	for _, f := range fragments {
		fmt.Fprintf(&cases, "  *%q*) exit 97 ;;\n", f)
	}
	script := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in\n%sesac\nexec %q \"$@\"\n", cases.String(), realGit)
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestSparseFirstCloneRefusesToPromoteWhenRollbackAlsoFails pins the
// fail-closed behavior for the one sparse-failure shape that is NOT safe to
// degrade past (review finding, W14-01).
//
// `sparse-checkout init --cone` narrows a never-sparse repo to top-level-only
// files as a side effect of merely enabling it. So if `set` fails and the
// `disable` rollback ALSO fails, the cone is still active and the following
// checkout populates almost nothing — for this fixture, whose root has no
// top-level tracked files, literally nothing.
//
// Nothing downstream can detect that: `git checkout` exits 0, HEAD resolves,
// and `git status --porcelain=v2` (what DirtyState parses) emits only branch
// headers, so the tree would be recorded available/CLEAN. Git reports
// "0% of tracked files present" only in human output, which DevStrap never
// reads. Hydrate must therefore FAIL rather than promote.
func TestSparseFirstCloneRefusesToPromoteWhenRollbackAlsoFails(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	remote := createSparseFixtureRemote(t)

	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "add", "file://"+remote, "--path", "work/acme/mono", "--default-branch", "main", "--sparse", "backend"); err != nil {
		t.Fatalf("add stderr = %q err = %v", stderr, err)
	}
	failGitCommandsContaining(t, "sparse-checkout set -- backend", "sparse-checkout disable")

	_, stderr, err := executeForTest("--home", home, "--root", root, "hydrate", "work/acme/mono")
	if err == nil {
		t.Fatalf("hydrate succeeded; it must refuse to promote a possibly-empty tree. stderr = %q", stderr)
	}
	if !strings.Contains(stderr, "refusing to promote") {
		t.Errorf("stderr = %q, want it to explain the refusal", stderr)
	}
	// The staging directory is cleaned up and nothing is promoted, so the
	// managed path must not exist as a repo at all — an empty-but-present
	// checkout is exactly the outcome being prevented.
	if _, statErr := os.Stat(filepath.Join(root, "work", "acme", "mono", ".git")); statErr == nil {
		t.Errorf("a repo was promoted at the managed path despite the refusal")
	}
}
