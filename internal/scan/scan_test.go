package scan

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/ignore"
)

func TestWalkPrunesGeneratedDirsWarnsSecretsSymlinkEscapesAndReportsDuplicates(t *testing.T) {
	root := t.TempDir()
	remote := "git@github.com:acme/api.git"
	initRepo(t, filepath.Join(root, "work", "api"), remote)
	initRepo(t, filepath.Join(root, "work", "api-copy"), remote)
	initRepo(t, filepath.Join(root, "node_modules", "vendored"), "git@github.com:acme/vendored.git")
	// Additional generated trees that must be pruned (TEST-1).
	mustMkdir(t, filepath.Join(root, "work", "api", ".venv", "lib"))
	mustMkdir(t, filepath.Join(root, "work", "svc", "target", "debug"))
	initRepo(t, filepath.Join(root, "work", "svc"), "git@github.com:acme/svc.git")
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("TOKEN=do-not-read"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	result, err := Walk(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Findings) != 3 {
		t.Fatalf("findings = %+v, want three real repos and no pruned vendored repo", result.Findings)
	}
	for _, finding := range result.Findings {
		if strings.Contains(finding.Path, "node_modules") || strings.Contains(finding.Path, ".venv") || strings.Contains(finding.Path, "target") {
			t.Fatalf("scanner descended into generated dir: %+v", result.Findings)
		}
	}
	// Findings, Duplicates, Secrets must be deterministically sorted.
	if !sort.SliceIsSorted(result.Findings, func(i, j int) bool { return result.Findings[i].Path < result.Findings[j].Path }) {
		t.Fatalf("findings not sorted: %+v", result.Findings)
	}
	if len(result.Duplicates) != 1 {
		t.Fatalf("duplicates = %+v, want one duplicate remote", result.Duplicates)
	}
	if got := result.Duplicates[0].RemoteKey; got != "github.com/acme/api" {
		t.Fatalf("duplicate remote key = %q", got)
	}
	if !hasWarning(result.Warnings, "secret-looking file found: .env") {
		t.Fatalf("warnings = %+v, want secret-looking file warning", result.Warnings)
	}
	if !hasWarning(result.Secrets, ".env") {
		t.Fatalf("secrets = %+v, want .env recorded", result.Secrets)
	}
	if !hasWarning(result.Warnings, "symlink escape (excluded): escape") {
		t.Fatalf("warnings = %+v, want symlink escape warning", result.Warnings)
	}
}

// TestWalkDoesNotPersistUnvalidatedRemote covers SEC-1: a dangerous origin URL
// must never end up in Finding.RemoteURL (which adopt would persist).
func TestWalkDoesNotPersistUnvalidatedRemote(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "work", "evil")
	mustMkdir(t, repo)
	runGit(t, repo, "init")
	// ext:: transport is the classic git RCE vector.
	runGit(t, repo, "remote", "add", "origin", "ext::sh -c touch% /tmp/pwned")

	result, err := Walk(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var evil *Finding
	for i := range result.Findings {
		if strings.Contains(result.Findings[i].Path, "evil") {
			evil = &result.Findings[i]
		}
	}
	if evil == nil {
		t.Fatalf("expected a finding for the evil repo: %+v", result.Findings)
	}
	if evil.RemoteURL != "" {
		t.Fatalf("unvalidated remote was persisted: %q", evil.RemoteURL)
	}
	if evil.RemoteKey != "" {
		t.Fatalf("unvalidated remote key was persisted: %q", evil.RemoteKey)
	}
	if !hasWarning(evil.Warnings, "ignoring unvalidated git remote") {
		t.Fatalf("expected unvalidated-remote warning, got %+v", evil.Warnings)
	}
}

func TestScanResolvesDefaultBranchOfflineAndWarns(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "work", "api")
	// Blackhole remote (RFC 5737 TEST-NET-1): a reintroduced scan-time network
	// call would hang past the sub-second elapsed budget below rather than
	// failing fast, making this a real no-network regression guard.
	initRepo(t, repo, "https://192.0.2.1/none.git")

	start := time.Now()
	result, err := Walk(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Walk took %s, want offline prompt return", elapsed)
	}

	var api *Finding
	for i := range result.Findings {
		if result.Findings[i].Path == "work/api" {
			api = &result.Findings[i]
		}
	}
	if api == nil {
		t.Fatalf("expected api finding: %+v", result.Findings)
	}
	if api.DefaultBranch != "main" {
		t.Fatalf("DefaultBranch = %q, want main", api.DefaultBranch)
	}
	if !hasWarning(api.Warnings, "resolved authoritatively at materialization") {
		t.Fatalf("expected materialization warning, got %+v", api.Warnings)
	}
}

func TestWalkCompilesDevstrapignoreAndPrunesCustomPatternWithDefaults(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".devstrapignore"), "vendor-drop/\n")
	initRepo(t, filepath.Join(root, "vendor-drop", "custom"), "git@github.com:acme/custom.git")
	initRepo(t, filepath.Join(root, "node_modules", "vendored"), "git@github.com:acme/vendored.git")
	initRepo(t, filepath.Join(root, "work", "api"), "git@github.com:acme/api.git")

	result, err := Walk(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Findings) != 1 || result.Findings[0].Path != "work/api" {
		t.Fatalf("findings = %+v, want only work/api", result.Findings)
	}
	if result.PrunedDirs != 2 {
		t.Fatalf("PrunedDirs = %d, want 2 (custom vendor-drop + default node_modules)", result.PrunedDirs)
	}
}

func TestWalkMalformedDevstrapignoreWarnsAndFallsBackToDefaults(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".devstrapignore"), "/\n")
	initRepo(t, filepath.Join(root, "node_modules", "vendored"), "git@github.com:acme/vendored.git")
	initRepo(t, filepath.Join(root, "work", "api"), "git@github.com:acme/api.git")

	result, err := Walk(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Findings) != 1 || result.Findings[0].Path != "work/api" {
		t.Fatalf("findings = %+v, want default-pruned node_modules and visible work/api", result.Findings)
	}
	if !hasWarning(result.Warnings, "ignore compile failed, using defaults") {
		t.Fatalf("warnings = %+v, want ignore compile warning", result.Warnings)
	}
	if !hasWarning(result.Warnings, "empty pattern after stripping prefix/suffix") {
		t.Fatalf("warnings = %+v, want parse error detail", result.Warnings)
	}
	if result.PrunedDirs != 1 {
		t.Fatalf("PrunedDirs = %d, want 1 (default node_modules pruned via fallback)", result.PrunedDirs)
	}
}

func TestWalkDevstrapignoreNegationReincludesDefaultPrunedDirectory(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".devstrapignore"), "!bin/\n")
	initRepo(t, filepath.Join(root, "bin", "somerepo"), "git@github.com:acme/somerepo.git")

	result, err := Walk(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Findings) != 1 {
		t.Fatalf("findings = %+v, want re-included bin repo", result.Findings)
	}
	if result.Findings[0].Path != "bin/somerepo" {
		t.Fatalf("finding path = %q, want bin/somerepo", result.Findings[0].Path)
	}
	if result.PrunedDirs != 0 {
		t.Fatalf("PrunedDirs = %d, want 0 when the negation re-includes the only prunable dir", result.PrunedDirs)
	}
}

// TestScanReportsSecretsThroughCanonicalPredicate pins the WIRING: the scanner
// reads ignore.IsSecretPath rather than a local clone. The detection table
// itself moved to internal/ignore's TestIsSecretPath when the two equivalent
// clones (this one and draftbundle's) were unified (AGEN-05).
//
// The .aws/credentials fixture is load-bearing: `credentials` is an
// unremarkable BASENAME, so only the path-level anchored-suffix half of
// IsSecretPath can catch it. Without it, swapping the call site for a bare
// IsSecretName(path.Base(...)) would still pass.
func TestScanReportsSecretsThroughCanonicalPredicate(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "work", "api")
	awsDir := filepath.Join(project, ".aws")
	if err := os.MkdirAll(awsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".env", ".env.example", "README.md"} {
		if err := os.WriteFile(filepath.Join(project, name), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(awsDir, "credentials"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Walk(context.Background(), root, Options{})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	want := []string{"work/api/.aws/credentials", "work/api/.env"}
	got := append([]string(nil), result.Secrets...)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("result.Secrets = %v, want %v (.env.example and README.md must not be reported)", got, want)
	}
}

// TestWalkEmitsTopmostPlainFolderAndNeverItsChildren covers NOVCS-02: a
// directory that is neither a git repo nor a recognized project becomes a
// `plain_folder`, and only the topmost one of a nested run does — recording
// `notes/` already says the path exists, so `notes/2026/jan` adds nothing.
func TestWalkEmitsTopmostPlainFolderAndNeverItsChildren(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "notes", "2026", "jan"))
	writeFile(t, filepath.Join(root, "notes", "2026", "jan", "standup.md"), "notes")
	mustMkdir(t, filepath.Join(root, "inbox"))

	result, err := Walk(context.Background(), root, Options{IncludeNonGit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := findingPaths(result, TypePlainFolder); !slices.Equal(got, []string{"inbox", "notes"}) {
		t.Fatalf("plain folders = %v, want [inbox notes] — only the topmost of a nested run", got)
	}
	for _, f := range result.Findings {
		if strings.HasPrefix(f.Path, "notes/") {
			t.Fatalf("child of a plain folder reported as its own finding: %+v", result.Findings)
		}
	}
}

// TestWalkNeverClassifiesAGroupingDirectoryAsPlainFolder is the regression this
// feature is one wrong line away from causing. `~/Code/work/acme/api-server` is
// the canonical managed tree: `work` and `work/acme` are bare directories with
// no manifest, so classifying a candidate on sight and skipping it would emit
// `work` as a plain_folder and never discover the repo underneath. Only a leaf
// area that groups nothing may be classified.
func TestWalkNeverClassifiesAGroupingDirectoryAsPlainFolder(t *testing.T) {
	root := t.TempDir()
	initRepo(t, filepath.Join(root, "work", "acme", "api-server"), "git@github.com:acme/api-server.git")
	mustMkdir(t, filepath.Join(root, "work", "acme", "scratch"))

	result, err := Walk(context.Background(), root, Options{IncludeNonGit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := findingPaths(result, TypeGitRepo); !slices.Equal(got, []string{"work/acme/api-server"}) {
		t.Fatalf("git repos = %v, want the nested repo to still be discovered", got)
	}
	if got := findingPaths(result, TypePlainFolder); !slices.Equal(got, []string{"work/acme/scratch"}) {
		t.Fatalf("plain folders = %v, want only the leaf area; a grouping dir must never be classified", got)
	}
}

// TestWalkNeverClassifiesAMaterializationSkeletonAsPlainFolder covers the other
// half of the same hazard. A project added but not yet hydrated is an empty
// skeleton on disk: indistinguishable from empty ground, so without the marker
// check its grouping parent would be emitted as a plain_folder and swallow a
// path the namespace already tracks as a git_repo.
func TestWalkNeverClassifiesAMaterializationSkeletonAsPlainFolder(t *testing.T) {
	root := t.TempDir()
	skeleton := filepath.Join(root, "work", "proj")
	mustMkdir(t, filepath.Join(skeleton, ".devstrap"))
	writeFile(t, filepath.Join(skeleton, ".devstrap", "placeholder.json"), `{"state":"skeleton"}`)
	writeFile(t, filepath.Join(skeleton, "README.devstrap.md"), "# DevStrap skeleton")

	result, err := Walk(context.Background(), root, Options{IncludeNonGit: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Findings) != 0 {
		t.Fatalf("findings = %+v, want none: a skeleton is an already-tracked project, and `work` above it is not empty ground", result.Findings)
	}
}

// TestWalkIncludeNonGitFalseClassifiesOnlyGitRepos pins the renamed option: one
// flag now gates both non-git classifications, so a git-only scan emits neither
// a draft_project nor a plain_folder.
func TestWalkIncludeNonGitFalseClassifiesOnlyGitRepos(t *testing.T) {
	root := t.TempDir()
	initRepo(t, filepath.Join(root, "repo"), "git@github.com:acme/api.git")
	mustMkdir(t, filepath.Join(root, "bare"))
	mustMkdir(t, filepath.Join(root, "manifested"))
	writeFile(t, filepath.Join(root, "manifested", "go.mod"), "module example.com/m\n")

	result, err := Walk(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := findingPaths(result, TypeGitRepo); !slices.Equal(got, []string{"repo"}) {
		t.Fatalf("findings = %+v, want only the git repo", result.Findings)
	}
	if len(result.Findings) != 1 {
		t.Fatalf("findings = %+v, want no non-git classifications", result.Findings)
	}
}

func TestPathAncestorsSplitsOnSegmentBoundaries(t *testing.T) {
	if got := pathAncestors("a/bc/d"); !slices.Equal(got, []string{"a", "a/bc"}) {
		t.Fatalf("pathAncestors = %v, want [a a/bc]", got)
	}
	if got := pathAncestors("solo"); len(got) != 0 {
		t.Fatalf("pathAncestors(solo) = %v, want none", got)
	}
	// "a/b" must not be read as an ancestor of "a/bc" by a bare prefix test.
	if hasAncestorIn(map[string]bool{"a/b": true}, "a/bc") {
		t.Fatal("a/b treated as an ancestor of a/bc — prefix test is not at a segment boundary")
	}
}

func findingPaths(result Result, want Type) []string {
	var paths []string
	for _, f := range result.Findings {
		if f.Type == want {
			paths = append(paths, f.Path)
		}
	}
	return paths
}

func TestShouldPruneDir(t *testing.T) {
	cases := []struct {
		name, rel string
		want      bool
	}{
		{".git", "work/api/.git", true},
		{"node_modules", "work/api/node_modules", true},
		{".venv", "work/api/.venv", true},
		{"venv", "work/api/venv", true},
		{"target", "work/svc/target", true},
		{"dist", "work/web/dist", true},
		{"__pycache__", "work/api/__pycache__", true},
		{"src", "work/api/src", false},
		{"internal", "work/api/internal", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ignore.DefaultMatcher().ShouldPruneDir(c.name, c.rel); got != c.want {
				t.Fatalf("shouldPruneDir(%q,%q)=%v want %v", c.name, c.rel, got, c.want)
			}
		})
	}
	// rel-suffix data dirs.
	if !ignore.DefaultMatcher().ShouldPruneDir("raw", "work/ml/data/raw") {
		t.Fatal("expected data/raw to be pruned")
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func initRepo(t *testing.T, path, remote string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, path, "init")
	runGit(t, path, "remote", "add", "origin", remote)
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func hasWarning(warnings []string, needle string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning, needle) {
			return true
		}
	}
	return false
}

// TestWalkCancelledMidWalkEmitsNoPlainFolder: only a completed walk can
// establish that a directory groups nothing. A cancelled walk still returns the
// findings it did make, and a candidate whose subtree it never reached would
// look like empty ground.
//
// The cancellation has to land MID-walk to prove anything. A pre-cancelled
// context aborts on the very first callback, before any candidate is recorded,
// so it would pass with the guard removed.
func TestWalkCancelledMidWalkEmitsNoPlainFolder(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "aaa"))
	mustMkdir(t, filepath.Join(root, "bbb"))

	// Calls 1 (the root) and 2 (aaa, recorded as a candidate) report healthy;
	// the walk is then interrupted at bbb.
	ctx := &cancelAfterNthCheck{Context: context.Background(), after: 2}
	result, err := Walk(ctx, root, Options{IncludeNonGit: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := findingPaths(result, TypePlainFolder); len(got) != 0 {
		t.Fatalf("plain folders = %v, want none: a partial walk cannot prove a directory groups nothing", got)
	}
}

// cancelAfterNthCheck reports Canceled once Err has been consulted more than
// `after` times, which is how the walk gets interrupted at a chosen directory
// rather than before it starts.
type cancelAfterNthCheck struct {
	context.Context
	after int
	calls int
}

func (c *cancelAfterNthCheck) Err() error {
	c.calls++
	if c.calls > c.after {
		return context.Canceled
	}
	return nil
}
