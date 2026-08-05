package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/manifest"
)

// setupManifestWorkspace builds a workspace holding one materialized git repo
// (work/acme/repo) and one non-git draft project (notes/scratch), so every
// export test exercises both halves of the honestly-scoped interop claim: the
// git subset a third-party tool can rebuild, and the rows it structurally
// cannot.
func setupManifestWorkspace(t *testing.T) (home, root string) {
	t.Helper()
	home = filepath.Join(t.TempDir(), ".devstrap")
	root = filepath.Join(t.TempDir(), "Code")
	setupFreshWorktreeRepo(t, home, root, "auto", false)

	scratch := filepath.Join(root, "notes", "scratch")
	if err := os.MkdirAll(scratch, 0o750); err != nil {
		t.Fatalf("create draft project dir: %v", err)
	}
	// scan classifies a marker-bearing, non-git directory as a draft_project.
	if err := os.WriteFile(filepath.Join(scratch, "README.md"), []byte("draft\n"), 0o600); err != nil {
		t.Fatalf("write draft project file: %v", err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "scan", root, "--adopt"); err != nil {
		t.Fatalf("scan --adopt stderr = %q err = %v", stderr, err)
	}
	return home, root
}

func exportForTest(t *testing.T, home, root string, extra ...string) (exportResult, string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "workspace.yaml")
	args := append([]string{"--home", home, "--root", root, "export", "--manifest", path, "--json"}, extra...)
	stdout, stderr, err := executeForTest(args...)
	if err != nil {
		t.Fatalf("export stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	var out exportResult
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("decode export --json: %v\n%s", err, stdout)
	}
	return out, path, stderr
}

func readManifest(t *testing.T, path string) manifest.Manifest {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // test-owned temp path.
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	m, err := manifest.Decode(raw)
	if err != nil {
		t.Fatalf("decode manifest: %v\n%s", err, raw)
	}
	return m
}

func TestExportWritesVCSToolSchemaWithScopedProjects(t *testing.T) {
	home, root := setupManifestWorkspace(t)
	result, path, _ := exportForTest(t, home, root)

	if result.SchemaVersion != manifest.SchemaVersion {
		t.Errorf("schema_version = %d, want %d", result.SchemaVersion, manifest.SchemaVersion)
	}
	// The two counts must not be conflated: `vcs import` rebuilds only the
	// first, and the manifest's own header says so.
	if result.Repositories != 1 {
		t.Errorf("repositories = %d, want 1 (only the git_repo)", result.Repositories)
	}
	if result.Projects != 2 {
		t.Errorf("projects = %d, want 2 (git_repo + draft_project)", result.Projects)
	}

	m := readManifest(t, path)
	repo, ok := m.Repositories["work/acme/repo"]
	if !ok {
		t.Fatalf("git_repo missing from `repositories`: %+v", m.Repositories)
	}
	if repo.Type != manifest.VCSTypeGit || repo.Version != "main" {
		t.Errorf("repositories entry = %+v, want type git version main", repo)
	}
	if _, ok := m.Repositories["notes/scratch"]; ok {
		t.Error("a draft_project must never appear under `repositories`: it has no url for `vcs import` to clone")
	}
	if got := m.DevStrap.Projects["notes/scratch"].Type; got != "draft_project" {
		t.Errorf("devstrap.projects[notes/scratch].type = %q, want draft_project", got)
	}
	if m.DevStrap.Pinned {
		t.Error("pinned must be false without --pinned")
	}
}

func TestExportManifestFileIsPrivateAndAtomic(t *testing.T) {
	home, root := setupManifestWorkspace(t)
	_, path, _ := exportForTest(t, home, root)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat manifest: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("manifest mode = %o, want 600", perm)
	}
	// No temp file may survive the promotion.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read manifest dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".devstrap-manifest-") {
			t.Errorf("temp file %s survived the atomic write", e.Name())
		}
	}
}

// TestExportStripsRemoteCredentials pins that a plaintext artifact meant to be
// copied off the machine never carries an embedded https credential.
func TestExportStripsRemoteCredentials(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "add",
		"https://token:s3cr3t@example.com/acme/api.git", "--path", "work/acme/api"); err != nil {
		t.Fatalf("add stderr = %q err = %v", stderr, err)
	}
	_, path, _ := exportForTest(t, home, root)
	raw, err := os.ReadFile(path) //nolint:gosec // test-owned temp path.
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if strings.Contains(string(raw), "s3cr3t") || strings.Contains(string(raw), "token:") {
		t.Fatalf("exported manifest leaked URL credentials:\n%s", raw)
	}
	if got := readManifest(t, path).Repositories["work/acme/api"].URL; got != "https://example.com/acme/api.git" {
		t.Errorf("url = %q, want the credential-free but still usable URL", got)
	}
}

// TestExportPinnedRecordsResolvedSHA covers the `vcs export --exact` mirror: a
// branch name is not a recovery artifact.
func TestExportPinnedRecordsResolvedSHA(t *testing.T) {
	home, root := setupManifestWorkspace(t)
	result, path, _ := exportForTest(t, home, root, "--pinned")
	if !result.Pinned {
		t.Error("pinned = false with --pinned")
	}
	head := strings.TrimSpace(runGitOutput(t, filepath.Join(root, "work", "acme", "repo"), "rev-parse", "HEAD"))
	if got := readManifest(t, path).Repositories["work/acme/repo"].Version; got != head {
		t.Errorf("version = %q, want the resolved HEAD %q", got, head)
	}
}

// TestExportPinnedOmitsVersionWhenUnresolvable is the honesty case: a project
// this device never materialized has no SHA to pin. The entry must carry NO
// version — vcstool then clones the remote default — rather than silently
// degrading to a branch name under a manifest that says `pinned: true`.
func TestExportPinnedOmitsVersionWhenUnresolvable(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "add",
		"https://example.com/acme/api.git", "--path", "work/acme/api", "--default-branch", "main"); err != nil {
		t.Fatalf("add stderr = %q err = %v", stderr, err)
	}
	result, path, stderr := exportForTest(t, home, root, "--pinned")
	if got := readManifest(t, path).Repositories["work/acme/api"].Version; got != "" {
		t.Errorf("version = %q, want empty for an unmaterialized project under --pinned", got)
	}
	if len(result.Warnings) == 0 {
		t.Error("an unpinnable project must be reported in the --json warnings array")
	}
	if !strings.Contains(stderr, "cannot pin") {
		t.Errorf("stderr must name the unpinnable project; got %q", stderr)
	}
}

// TestExportDiagnosticsNeverReachStdout pins the machine-contract invariant:
// stdout carries exactly one JSON document, every warning goes to stderr.
func TestExportDiagnosticsNeverReachStdout(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "add",
		"https://example.com/acme/api.git", "--path", "work/acme/api"); err != nil {
		t.Fatalf("add stderr = %q err = %v", stderr, err)
	}
	path := filepath.Join(t.TempDir(), "workspace.yaml")
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "export", "--manifest", path, "--pinned", "--json")
	if err != nil {
		t.Fatalf("export err = %v stderr = %q", err, stderr)
	}
	if !strings.Contains(stderr, "warning:") {
		t.Fatalf("expected a warning on stderr, got %q", stderr)
	}
	var out exportResult
	dec := json.NewDecoder(strings.NewReader(stdout))
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
	}
	if dec.More() {
		t.Fatalf("stdout carries more than one document:\n%s", stdout)
	}
}

func TestExportRequiresManifestFlag(t *testing.T) {
	home, root := setupManifestWorkspace(t)
	_, stderr, err := executeForTest("--home", home, "--root", root, "export")
	if err == nil {
		t.Fatal("want a usage error without --manifest")
	}
	if code := ExitCodeWithWriter(err, &strings.Builder{}); code != exitUsage {
		t.Errorf("exit code = %d, want %d; stderr = %q", code, exitUsage, stderr)
	}
}

// TestExportPinnedOmitsUnpushedHead pins the review's MAJOR-1. `--pinned` is
// documented as the flag to use when the manifest IS a recovery artifact — which
// is exactly the case where an unpushed HEAD pins a SHA that, after total local
// loss, exists nowhere. `vcs import` would fail its checkout during the actual
// recovery.
//
// The manifest must degrade to no version rather than record a pin it cannot
// vouch for, the same way it already handles an unresolvable HEAD.
func TestExportPinnedOmitsUnpushedHead(t *testing.T) {
	home, root := setupManifestWorkspace(t)
	repo := filepath.Join(root, "work", "acme", "repo")

	// Precondition: with everything pushed, the pin resolves.
	if _, path, _ := exportForTest(t, home, root, "--pinned"); readManifest(t, path).Repositories["work/acme/repo"].Version == "" {
		t.Fatal("precondition failed: a fully pushed HEAD should pin")
	}

	// Commit locally without pushing. rev-parse resolves the SHA happily; it is
	// simply not anywhere a remote can serve it from.
	if err := os.WriteFile(filepath.Join(repo, "unpushed.txt"), []byte("local only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitOutput(t, repo, "add", "-A")
	runGitOutput(t, repo, "commit", "-m", "unpushed")

	result, path, stderr := exportForTest(t, home, root, "--pinned")
	if got := readManifest(t, path).Repositories["work/acme/repo"].Version; got != "" {
		t.Fatalf("manifest pinned an UNPUSHED sha %q; after total local loss that commit exists "+
			"nowhere and `vcs import` cannot check it out", got)
	}
	if len(result.Warnings) == 0 {
		t.Error("an unpinnable project must be reported in the --json warnings array")
	}
	if !strings.Contains(stderr, "cannot pin") {
		t.Errorf("stderr must name the unpinnable project; got %q", stderr)
	}
}

// TestExportPinnedScopesReachabilityToTheExportedRemote pins P11-MANIFEST-01.
//
// A manifest entry pairs its SHA with exactly ONE url — the registered remote,
// exported as this entry's `url`. So "is this SHA on some remote I have
// fetched" is the wrong question to gate the pin on. The fork workflow makes
// the gap load-bearing: `origin` is an empty fork and the canonical repo is a
// second remote holding the commit. The unscoped question answers yes, the
// manifest pins the SHA against the FORK's url, and `vcs import` clones the
// fork and fails its checkout during the actual disaster recovery — precisely
// what the reachability gate was added to prevent.
func TestExportPinnedScopesReachabilityToTheExportedRemote(t *testing.T) {
	home, root := setupManifestWorkspace(t)
	repo := filepath.Join(root, "work", "acme", "repo")

	// Non-regression half: the ordinary single-remote case still pins.
	if _, path, _ := exportForTest(t, home, root, "--pinned"); readManifest(t, path).Repositories["work/acme/repo"].Version == "" {
		t.Fatal("precondition failed: a fully pushed HEAD on the registered remote should pin")
	}

	// `origin` stays the registered remote — it is the url the manifest
	// exports, and it is the one that will NOT hold the commit.
	upstream := filepath.Join(t.TempDir(), "upstream.git")
	runGitOutput(t, filepath.Dir(upstream), "init", "--bare", upstream)
	runGitOutput(t, repo, "remote", "add", "upstream", "file://"+upstream)

	if err := os.WriteFile(filepath.Join(repo, "upstream-only.txt"), []byte("canonical\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitOutput(t, repo, "add", "-A")
	runGitOutput(t, repo, "commit", "-m", "lands on upstream, never on the registered origin")
	runGitOutput(t, repo, "push", "upstream", "HEAD:refs/heads/main")
	runGitOutput(t, repo, "fetch", "upstream")

	result, path, stderr := exportForTest(t, home, root, "--pinned")
	if got := readManifest(t, path).Repositories["work/acme/repo"].Version; got != "" {
		t.Fatalf("manifest pinned %q against the exported remote's url, which does not contain it — "+
			"the commit is only on `upstream`, so `vcs import` would fail its checkout during recovery", got)
	}
	if len(result.Warnings) == 0 {
		t.Error("an unpinnable project must be reported in the --json warnings array")
	}
	if !strings.Contains(stderr, "cannot pin") {
		t.Errorf("stderr must name the unpinnable project; got %q", stderr)
	}

	// Once the exported remote genuinely serves the commit the pin returns: the
	// gate is scoped to the right remote, not merely made stricter.
	runGitOutput(t, repo, "push", "origin", "HEAD:refs/heads/main")
	runGitOutput(t, repo, "fetch", "origin")
	head := strings.TrimSpace(runGitOutput(t, repo, "rev-parse", "HEAD"))
	if _, path, _ := exportForTest(t, home, root, "--pinned"); readManifest(t, path).Repositories["work/acme/repo"].Version != head {
		t.Error("a SHA the exported remote genuinely serves must pin")
	}
}

// TestExportPinnedRefusesWhenARemoteIsNestedInsideTheExportedOne closes the
// escape hatch under the scoping itself (Codex review of P11-MANIFEST-01). A
// remote NAME may contain a slash, and `origin/vendor` writes its refs to
// refs/remotes/origin/vendor/* — whose shortened names match the `origin/*`
// pattern, because git's wildmatch runs without WM_PATHNAME and `*` spans
// slashes. A commit present only on the nested remote would therefore vouch for
// origin's url, reopening the exact hole the scoping closes. The namespaces are
// indistinguishable by pattern, so the pin is refused rather than guessed.
//
// The remote is written with `git config` rather than `git remote add` because
// git's porcelain guard against nested names is VERSION-DEPENDENT: 2.50.1
// creates this happily, while the CI runners' newer git refuses with "remote
// name 'origin/vendor' is a subset of existing remote 'origin'". The state is
// reachable on every version through config (and through `remote add` on older
// git, or in a repo created by one), so the guard is real — but the test must
// not depend on which git built the fixture.
func TestExportPinnedRefusesWhenARemoteIsNestedInsideTheExportedOne(t *testing.T) {
	home, root := setupManifestWorkspace(t)
	repo := filepath.Join(root, "work", "acme", "repo")

	nested := filepath.Join(t.TempDir(), "vendor.git")
	runGitOutput(t, filepath.Dir(nested), "init", "--bare", nested)
	runGitOutput(t, repo, "config", "remote.origin/vendor.url", "file://"+nested)
	runGitOutput(t, repo, "config", "remote.origin/vendor.fetch", "+refs/heads/*:refs/remotes/origin/vendor/*")

	if err := os.WriteFile(filepath.Join(repo, "vendor-only.txt"), []byte("vendored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitOutput(t, repo, "add", "-A")
	runGitOutput(t, repo, "commit", "-m", "lands only on the nested remote")
	runGitOutput(t, repo, "push", "origin/vendor", "HEAD:refs/heads/main")
	runGitOutput(t, repo, "fetch", "origin/vendor")

	// Precondition: the nested remote's ref really is what `origin/*` would
	// match, so this test would be vacuous without the refusal.
	if got := runGitOutput(t, repo, "branch", "-r", "--list", "origin/*", "--format=%(refname)"); !strings.Contains(got, "origin/vendor/main") {
		t.Fatalf("precondition failed: `origin/*` no longer matches the nested remote's refs; got %q", got)
	}

	result, path, stderr := exportForTest(t, home, root, "--pinned")
	if got := readManifest(t, path).Repositories["work/acme/repo"].Version; got != "" {
		t.Fatalf("manifest pinned %q vouched for by a NESTED remote's refs; origin itself does not serve it", got)
	}
	if len(result.Warnings) == 0 {
		t.Error("an unpinnable project must be reported in the --json warnings array")
	}
	if !strings.Contains(stderr, "nested inside") {
		t.Errorf("stderr must explain the nested-remote refusal; got %q", stderr)
	}
}
