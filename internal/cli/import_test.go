package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
)

func writeManifestForTest(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "workspace.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}

func importForTest(t *testing.T, home, root, path string) (importResult, string, error) {
	t.Helper()
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "import", "--manifest", path, "--json")
	var out importResult
	if stdout != "" {
		if jsonErr := json.Unmarshal([]byte(stdout), &out); jsonErr != nil {
			t.Fatalf("decode import --json: %v\n%s", jsonErr, stdout)
		}
	}
	return out, stderr, err
}

func projectsForTest(t *testing.T, home, root string) map[string]state.ProjectStatus {
	t.Helper()
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "status", "--json")
	if err != nil {
		t.Fatalf("status stderr = %q err = %v", stderr, err)
	}
	var summary state.Summary
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil {
		t.Fatalf("decode status --json: %v\n%s", err, stdout)
	}
	out := make(map[string]state.ProjectStatus, len(summary.Projects))
	for _, p := range summary.Projects {
		out[p.Path] = p
	}
	return out
}

func initForTest(t *testing.T, home, root string) {
	t.Helper()
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
}

// TestManifestRoundTripRecoversNamespaceAfterTotalLoss is the AD-7 drill in
// process: export, destroy the state dir, re-init, import, and assert the
// namespace map came back equivalent.
func TestManifestRoundTripRecoversNamespaceAfterTotalLoss(t *testing.T) {
	home, root := setupManifestWorkspace(t)
	before := projectsForTest(t, home, root)
	if len(before) != 2 {
		t.Fatalf("setup produced %d projects, want 2", len(before))
	}
	_, path, _ := exportForTest(t, home, root)

	// Total local loss: the state dir is gone, the manifest is all that is left.
	if err := os.RemoveAll(home); err != nil {
		t.Fatalf("wipe state dir: %v", err)
	}
	initForTest(t, home, root)
	if got := projectsForTest(t, home, root); len(got) != 0 {
		t.Fatalf("re-initialized workspace already holds %d projects", len(got))
	}

	result, stderr, err := importForTest(t, home, root, path)
	if err != nil {
		t.Fatalf("import stderr = %q err = %v", stderr, err)
	}
	if result.Registered != 2 || result.Skipped != 0 {
		t.Fatalf("import result = %+v, want 2 registered / 0 skipped", result)
	}

	after := projectsForTest(t, home, root)
	if len(after) != len(before) {
		t.Fatalf("recovered %d projects, want %d", len(after), len(before))
	}
	for path, want := range before {
		got, ok := after[path]
		if !ok {
			t.Errorf("project %s was not recovered", path)
			continue
		}
		if got.Type != want.Type {
			t.Errorf("%s type = %q, want %q", path, got.Type, want.Type)
		}
		if got.RemoteKey != want.RemoteKey {
			t.Errorf("%s remote_key = %q, want %q", path, got.RemoteKey, want.RemoteKey)
		}
		if got.DefaultBranch != want.DefaultBranch {
			t.Errorf("%s default_branch = %q, want %q", path, got.DefaultBranch, want.DefaultBranch)
		}
		if got.LFSPolicy != want.LFSPolicy {
			t.Errorf("%s lfs_policy = %q, want %q", path, got.LFSPolicy, want.LFSPolicy)
		}
	}
}

// TestImportRegistersWithoutMaterializing pins the plane boundary: import writes
// rows and stops. A second cloning path is exactly what spec/13 refused for
// /v1/status, so the recovered rows must land in `skeleton` — the state that
// hands them to the ONE existing materialize pass.
func TestImportRegistersWithoutMaterializing(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	initForTest(t, home, root)
	path := writeManifestForTest(t, `
repositories:
  work/acme/api:
    type: git
    url: https://example.com/acme/api.git
    version: main
devstrap:
  schema_version: 1
  workspace_id: ws_01
  exported_at: "2026-08-01T00:00:00Z"
  pinned: false
  projects:
    work/acme/api:
      type: git_repo
      default_branch: main
`)
	if _, stderr, err := importForTest(t, home, root, path); err != nil {
		t.Fatalf("import stderr = %q err = %v", stderr, err)
	}
	project, ok := projectsForTest(t, home, root)["work/acme/api"]
	if !ok {
		t.Fatal("the project was not registered")
	}
	if project.MaterializationState == "available" {
		t.Errorf("materialization_state = %q; import must not claim a checkout it never made", project.MaterializationState)
	}
	// Nothing may have been created on disk: the URL above is unreachable, so
	// any clone attempt would also have failed loudly.
	if _, err := os.Stat(filepath.Join(root, "work", "acme", "api")); !os.IsNotExist(err) {
		t.Errorf("import touched the managed root: stat err = %v", err)
	}
}

// TestImportedProjectMaterializesThroughTheExistingPass is the other half of
// the plane boundary: having registered and stopped, the imported row must be
// picked up by the ONE existing materialize pass with no import-specific
// cloning code involved.
func TestImportedProjectMaterializesThroughTheExistingPass(t *testing.T) {
	source, remote := filepath.Join(t.TempDir(), ".devstrap"), ""
	{
		// Build a real bare remote by reusing the shared worktree fixture, then
		// read the remote URL back out of the manifest it exports.
		sourceRoot := filepath.Join(t.TempDir(), "Code")
		setupFreshWorktreeRepo(t, source, sourceRoot, "auto", false)
		_, path, _ := exportForTest(t, source, sourceRoot)
		remote = readManifest(t, path).Repositories["work/acme/repo"].URL
	}
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	initForTest(t, home, root)
	path := writeManifestForTest(t, "repositories:\n  work/acme/repo:\n    type: git\n    url: "+remote+"\n    version: main\n")
	if _, stderr, err := importForTest(t, home, root, path); err != nil {
		t.Fatalf("import stderr = %q err = %v", stderr, err)
	}
	if _, err := os.Stat(filepath.Join(root, "work", "acme", "repo", ".git")); !os.IsNotExist(err) {
		t.Fatalf("import cloned on its own: stat err = %v", err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "materialize"); err != nil {
		t.Fatalf("materialize stderr = %q err = %v", stderr, err)
	}
	if _, err := os.Stat(filepath.Join(root, "work", "acme", "repo", ".git")); err != nil {
		t.Fatalf("materialize did not clone the imported project: %v", err)
	}
}

func TestImportIsIdempotent(t *testing.T) {
	home, root := setupManifestWorkspace(t)
	_, path, _ := exportForTest(t, home, root)
	result, stderr, err := importForTest(t, home, root, path)
	if err != nil {
		t.Fatalf("import stderr = %q err = %v", stderr, err)
	}
	if result.Registered != 0 || result.AlreadyPresent != 2 || result.Skipped != 0 {
		t.Fatalf("re-importing an already-registered namespace = %+v, want 0/2/0", result)
	}
}

// TestImportSkipsForeignVCSTypeAndExitsNonZero: vcstool also speaks hg/svn/bzr.
// Registering one would create a row that can never materialize, so it is
// skipped — and the exit code says the import was partial, mirroring
// ErrPartialMaterialize rather than reporting a whole recovery.
func TestImportSkipsForeignVCSTypeAndExitsNonZero(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	initForTest(t, home, root)
	path := writeManifestForTest(t, `
repositories:
  legacy/hg-thing:
    type: hg
    url: https://example.com/hg/thing
    version: default
  work/acme/api:
    type: git
    url: https://example.com/acme/api.git
    version: main
`)
	result, stderr, err := importForTest(t, home, root, path)
	if err == nil {
		t.Fatal("a partial import must exit non-zero")
	}
	if result.Registered != 1 || result.Skipped != 1 {
		t.Fatalf("import result = %+v, want 1 registered / 1 skipped", result)
	}
	if !strings.Contains(stderr, `repository type "hg" is not supported`) {
		t.Errorf("stderr must name the unsupported type; got %q", stderr)
	}
	if _, ok := projectsForTest(t, home, root)["legacy/hg-thing"]; ok {
		t.Error("a mercurial entry must not be registered as a project")
	}
}

func TestImportRefusesUnsafeNamespacePath(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	initForTest(t, home, root)
	path := writeManifestForTest(t, `
repositories:
  ../../etc/evil:
    type: git
    url: https://example.com/acme/api.git
    version: main
`)
	result, stderr, err := importForTest(t, home, root, path)
	if err == nil {
		t.Fatal("an unsafe namespace path must not import cleanly")
	}
	if result.Registered != 0 || result.Skipped != 1 {
		t.Fatalf("import result = %+v, want 0 registered / 1 skipped", result)
	}
	if !strings.Contains(stderr, "unsafe namespace path") {
		t.Errorf("stderr must explain the refusal; got %q", stderr)
	}
}

// TestImportAcceptsBareReposFile proves interop runs both ways: a `.repos` file
// with no `devstrap` key — written by hand or by `vcs export` — is importable,
// and its `version` is adopted as the default branch.
func TestImportAcceptsBareReposFile(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	initForTest(t, home, root)
	path := writeManifestForTest(t, "repositories:\n  oss/tool:\n    type: git\n    url: https://example.com/x/tool.git\n    version: develop\n")
	result, stderr, err := importForTest(t, home, root, path)
	if err != nil {
		t.Fatalf("import stderr = %q err = %v", stderr, err)
	}
	if result.Registered != 1 {
		t.Fatalf("import result = %+v, want 1 registered", result)
	}
	got := projectsForTest(t, home, root)["oss/tool"]
	if got.Type != "git_repo" {
		t.Errorf("type = %q, want git_repo", got.Type)
	}
	if got.DefaultBranch != "develop" {
		t.Errorf("default_branch = %q, want develop (a bare .repos file carries the branch only in `version`)", got.DefaultBranch)
	}
}

// TestImportNeverTreatsAPinnedSHAAsABranch: `version` holds a commit id in a
// pinned export (and can in a hand-written .repos file). Recording it as a
// default branch would break every later fetch.
func TestImportNeverTreatsAPinnedSHAAsABranch(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	initForTest(t, home, root)
	path := writeManifestForTest(t, "repositories:\n  oss/tool:\n    type: git\n    url: https://example.com/x/tool.git\n    version: 0123456789abcdef0123456789abcdef01234567\n")
	if _, stderr, err := importForTest(t, home, root, path); err != nil {
		t.Fatalf("import stderr = %q err = %v", stderr, err)
	}
	if got := projectsForTest(t, home, root)["oss/tool"].DefaultBranch; got != "main" {
		t.Errorf("default_branch = %q, want the \"main\" fallback rather than a commit id", got)
	}
}

// TestImportRefusesToOverwriteADifferentProject: import is a recovery plane, so
// a stale manifest must never silently rewrite live state.
func TestImportRefusesToOverwriteADifferentProject(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	initForTest(t, home, root)
	if _, stderr, err := executeForTest("--home", home, "--root", root, "add",
		"https://example.com/acme/api.git", "--path", "work/acme/api"); err != nil {
		t.Fatalf("add stderr = %q err = %v", stderr, err)
	}
	path := writeManifestForTest(t, "repositories:\n  work/acme/api:\n    type: git\n    url: https://example.com/someone-else/api.git\n    version: main\n")
	result, stderr, err := importForTest(t, home, root, path)
	if err == nil {
		t.Fatal("a conflicting entry must exit non-zero")
	}
	if result.Skipped != 1 || result.Registered != 0 {
		t.Fatalf("import result = %+v, want 0 registered / 1 skipped", result)
	}
	if !strings.Contains(stderr, "refusing to overwrite") {
		t.Errorf("stderr must explain the refusal; got %q", stderr)
	}
	if got := projectsForTest(t, home, root)["work/acme/api"].RemoteURL; got != "https://example.com/acme/api.git" {
		t.Errorf("the live remote was rewritten to %q", got)
	}
}

func TestImportWarnsOnForeignWorkspaceID(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	initForTest(t, home, root)
	path := writeManifestForTest(t, `
repositories: {}
devstrap:
  schema_version: 1
  workspace_id: ws_01jz0000000000000000000000
  exported_at: "2026-08-01T00:00:00Z"
  pinned: false
  projects: {}
`)
	result, stderr, err := importForTest(t, home, root, path)
	if err != nil {
		t.Fatalf("import stderr = %q err = %v", stderr, err)
	}
	if !strings.Contains(stderr, "was exported from workspace") {
		t.Errorf("stderr must warn about the workspace mismatch; got %q", stderr)
	}
	if len(result.Warnings) == 0 {
		t.Error("the workspace mismatch must also appear in the --json warnings array")
	}
}

// TestImportReadsANewerSchemaVersion: evolution is additive-only, so a manifest
// from a newer DevStrap is read (with a warning), never refused — refusing
// would break recovery precisely when it matters.
func TestImportReadsANewerSchemaVersion(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	initForTest(t, home, root)
	path := writeManifestForTest(t, `
repositories:
  oss/tool:
    type: git
    url: https://example.com/x/tool.git
    version: main
    unknown_attribute: ignored
devstrap:
  schema_version: 99
  workspace_id: ws_01
  exported_at: "2026-08-01T00:00:00Z"
  pinned: false
  projects:
    oss/tool:
      type: git_repo
      default_branch: main
      unknown_field: 7
`)
	result, stderr, err := importForTest(t, home, root, path)
	if err != nil {
		t.Fatalf("import stderr = %q err = %v", stderr, err)
	}
	if result.Registered != 1 {
		t.Fatalf("import result = %+v, want 1 registered", result)
	}
	if result.ManifestSchemaVersion != 99 {
		t.Errorf("manifest_schema_version = %d, want 99", result.ManifestSchemaVersion)
	}
	if !strings.Contains(stderr, "schema_version 99") {
		t.Errorf("stderr must report the newer schema version; got %q", stderr)
	}
}

func TestImportRejectsANonManifestFile(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	initForTest(t, home, root)
	path := writeManifestForTest(t, "some_other_tool:\n  key: value\n")
	_, stderr, err := executeForTest("--home", home, "--root", root, "import", "--manifest", path)
	if err == nil {
		t.Fatal("want an error importing a file that is not a workspace manifest")
	}
	if code := ExitCodeWithWriter(err, &strings.Builder{}); code != exitInvalidConfig {
		t.Errorf("exit code = %d, want %d; stderr = %q", code, exitInvalidConfig, stderr)
	}
}

func TestImportRequiresManifestFlag(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	initForTest(t, home, root)
	_, _, err := executeForTest("--home", home, "--root", root, "import")
	if err == nil {
		t.Fatal("want a usage error without --manifest")
	}
	if code := ExitCodeWithWriter(err, &strings.Builder{}); code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
}

// TestImportDiagnosticsNeverReachStdout pins the machine-contract invariant for
// the import half: warnings on stderr, exactly one JSON document on stdout.
func TestImportDiagnosticsNeverReachStdout(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	initForTest(t, home, root)
	path := writeManifestForTest(t, "repositories:\n  legacy/hg:\n    type: hg\n    url: https://example.com/hg\n")
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "import", "--manifest", path, "--json")
	if err == nil {
		t.Fatal("a fully skipped import must exit non-zero")
	}
	if !strings.Contains(stderr, "warning:") {
		t.Fatalf("expected a warning on stderr, got %q", stderr)
	}
	var out importResult
	dec := json.NewDecoder(strings.NewReader(stdout))
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
	}
	if dec.More() {
		t.Fatalf("stdout carries more than one document:\n%s", stdout)
	}
}
