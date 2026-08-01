package manifest

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"
)

// goldenFixture is the manifest the golden file pins. It deliberately covers
// every project type plus every optional field, so a change to the emitted
// schema cannot slip through by only touching a shape the fixture omits.
func goldenFixture() Manifest {
	return Manifest{
		Repositories: map[string]RepoEntry{
			"work/acme/api": {Type: "git", URL: "git@github.com:acme/api.git", Version: "main"},
			"oss/tool":      {Type: "git", URL: "https://github.com/example/tool.git", Version: "trunk"},
		},
		DevStrap: Section{
			SchemaVersion: SchemaVersion,
			WorkspaceID:   "ws_01jz0000000000000000000000",
			WorkspaceName: "Code",
			ExportedAt:    "2026-08-01T00:00:00Z",
			ExportedBy:    "dev_01jz0000000000000000000000",
			Pinned:        false,
			Projects: map[string]Project{
				"work/acme/api": {
					Type: "git_repo", DefaultBranch: "main", LFSPolicy: "auto",
					ForgeKind: "github", MaterializationPolicy: "lazy", EnvProfile: true,
				},
				"oss/tool":      {Type: "git_repo", DefaultBranch: "trunk", LFSPolicy: "never", MaterializationPolicy: "lazy"},
				"notes/scratch": {Type: "plain_folder", MaterializationPolicy: "lazy"},
				"drafts/idea":   {Type: "draft_project", MaterializationPolicy: "lazy"},
				"vendor/patched": {
					Type: "local_git", DefaultBranch: "main", MaterializationPolicy: "lazy",
				},
			},
		},
	}
}

const goldenPath = "testdata/workspace.golden.yaml"

// TestEncodeMatchesGolden pins the exact emitted bytes. The golden file is the
// artifact a third-party tool consumes, so any change to it is a change to a
// published format and must be a deliberate edit of this file, not a side
// effect. Regenerate with `go test ./internal/manifest -update-golden`.
func TestEncodeMatchesGolden(t *testing.T) {
	got, err := Encode(goldenFixture())
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if updateGolden() {
		if err := os.WriteFile(goldenPath, got, 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Fatalf("golden file regenerated; re-run without -update-golden")
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("emitted manifest differs from %s.\n--- got ---\n%s\n--- want ---\n%s", goldenPath, got, want)
	}
}

var updateGoldenFlag = flag.Bool("update-golden", false, "rewrite testdata/workspace.golden.yaml from the current encoder")

func updateGolden() bool { return *updateGoldenFlag }

// TestGoldenStaysVCSToolCompatible is the assertion that actually protects the
// interop claim, and it deliberately does NOT decode through this package's own
// structs — doing so would validate the implementation against itself. It reads
// the golden file as a generic YAML tree and asserts the three properties
// vcstool's parser depends on (vcstool/commands/import_.py,
// get_repos_in_vcstool_format): a root `repositories` mapping, and inside every
// entry the `type` and `url` keys it requires.
func TestGoldenStaysVCSToolCompatible(t *testing.T) {
	raw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var root map[string]any
	if err := yaml.Unmarshal(raw, &root); err != nil {
		t.Fatalf("golden is not valid YAML: %v", err)
	}

	repositories, ok := root["repositories"].(map[string]any)
	if !ok {
		t.Fatalf("golden has no root `repositories` mapping; `vcs import` raises \"Input data is not valid format\". root keys = %v", keysOf(root))
	}
	if len(repositories) == 0 {
		t.Fatal("golden `repositories` is empty; the fixture no longer exercises the interop path")
	}
	// vcstool reads exactly these three attributes and ignores the rest. The
	// assertion is that DevStrap fields never appear HERE: they belong under
	// the sibling `devstrap` key, so our evolution and vcstool's parser never
	// share a namespace.
	allowed := map[string]bool{"type": true, "url": true, "version": true}
	for path, value := range repositories {
		entry, ok := value.(map[string]any)
		if !ok {
			t.Errorf("repositories[%q] is not a mapping", path)
			continue
		}
		for _, required := range []string{"type", "url"} {
			if _, ok := entry[required]; !ok {
				t.Errorf("repositories[%q] has no %q; vcstool skips such an entry with a warning", path, required)
			}
		}
		for key := range entry {
			if !allowed[key] {
				t.Errorf("repositories[%q] carries non-vcstool key %q; DevStrap fields belong under the `devstrap` key", path, key)
			}
		}
		if got := entry["type"]; got != VCSTypeGit {
			t.Errorf("repositories[%q].type = %v, want %q", path, got, VCSTypeGit)
		}
	}

	section, ok := root["devstrap"].(map[string]any)
	if !ok {
		t.Fatal("golden has no top-level `devstrap` mapping")
	}
	if _, ok := section["schema_version"]; !ok {
		t.Error("`devstrap` carries no schema_version")
	}
	if _, ok := root["devstrap"].(map[string]any)["projects"]; !ok {
		t.Error("`devstrap` carries no projects map")
	}
}

// TestGoldenHeaderScopesTheInteropClaim pins the honesty of the emitted header:
// the file must say, in itself, that only the git projects are third-party
// recoverable and that no encrypted content travels with it. A user reading
// this artifact mid-disaster must not have to find the spec.
func TestGoldenHeaderScopesTheInteropClaim(t *testing.T) {
	raw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	text := string(raw)
	for _, want := range []string{
		"vcs import",
		"devstrap import --manifest",
		"and ONLY those",
		"structurally invisible",
		"NO SECRETS",
		"never carries one",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("emitted header no longer contains %q; the scoping/secret disclaimer is part of the artifact, not just the spec", want)
		}
	}
}

func TestEncodeIsDeterministic(t *testing.T) {
	first, err := Encode(goldenFixture())
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	for range 8 {
		again, err := Encode(goldenFixture())
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		if string(again) != string(first) {
			t.Fatal("Encode is not deterministic across runs; a golden assertion would be flaky and two exports of one namespace would differ")
		}
	}
}

func TestRoundTrip(t *testing.T) {
	want := goldenFixture()
	raw, err := Encode(want)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip lost data.\ngot  %+v\nwant %+v", got, want)
	}
}

// TestDecodeIgnoresUnknownKeys is the consumer half of the SchemaVersion
// contract: keys added by a newer DevStrap must not break an older one.
func TestDecodeIgnoresUnknownKeys(t *testing.T) {
	raw := []byte(`
repositories:
  work/api:
    type: git
    url: git@example.com:acme/api.git
    version: main
    future_attribute: ignored
devstrap:
  schema_version: 99
  workspace_id: ws_1
  exported_at: "2026-08-01T00:00:00Z"
  pinned: false
  future_section:
    anything: goes
  projects:
    work/api:
      type: git_repo
      default_branch: main
      future_field: 12
`)
	m, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if m.DevStrap.SchemaVersion != 99 {
		t.Errorf("schema_version = %d, want 99", m.DevStrap.SchemaVersion)
	}
	if got := m.Repositories["work/api"].URL; got != "git@example.com:acme/api.git" {
		t.Errorf("url = %q", got)
	}
	if got := m.DevStrap.Projects["work/api"].Type; got != "git_repo" {
		t.Errorf("type = %q", got)
	}
}

// TestDecodeAcceptsBareReposFile proves interop runs both ways: a `.repos` file
// written by hand or by `vcs export` — no `devstrap` key at all — is importable.
func TestDecodeAcceptsBareReposFile(t *testing.T) {
	m, err := Decode([]byte("repositories:\n  a/b:\n    type: git\n    url: https://example.com/b.git\n    version: dev\n"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(m.Repositories) != 1 || m.DevStrap.SchemaVersion != 0 || len(m.DevStrap.Projects) != 0 {
		t.Fatalf("unexpected decode of a bare .repos file: %+v", m)
	}
}

func TestDecodeRejectsNonManifest(t *testing.T) {
	for name, raw := range map[string]string{
		"unrelated mapping": "some_other_tool:\n  key: value\n",
		"empty document":    "",
		"a list":            "- one\n- two\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode([]byte(raw)); err == nil {
				t.Fatal("Decode accepted a document that is not a workspace manifest")
			}
		})
	}
}

func TestEncodeEmptyNamespace(t *testing.T) {
	raw, err := Encode(Manifest{DevStrap: Section{SchemaVersion: SchemaVersion, WorkspaceID: "ws_1", ExportedAt: "2026-08-01T00:00:00Z"}})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !strings.Contains(string(raw), "repositories: {}") {
		t.Errorf("an empty namespace must still emit the root key vcstool requires; got:\n%s", raw)
	}
	if _, err := Decode(raw); err != nil {
		t.Fatalf("Decode of an empty manifest: %v", err)
	}
}

// TestVCSToolImportsEmittedManifest runs the REAL third-party tool against a
// manifest this package emitted, which is the only way the interop claim is
// evidence rather than assertion. It clones from local bare repos, so it needs
// no network. It skips when vcstool is not installed (`uv tool install
// vcstool`, or pipx/pip) — the golden-file assertions above are the gate that
// always runs.
func TestVCSToolImportsEmittedManifest(t *testing.T) {
	vcs, err := exec.LookPath("vcs-import")
	if err != nil {
		// REFUSE rather than skip when the environment claims to have installed
		// vcstool. A silently-skipping test reads exactly like a passing one,
		// which is how this test came to verify nothing in CI at all
		// (W13-02 review) — and it is the same defect W13-06's Secret Service
		// job was written to prevent. The CI step that installs vcstool sets
		// this variable, so a broken install fails loudly instead of quietly
		// restoring the state the install was added to fix.
		if os.Getenv("DEVSTRAP_REQUIRE_VCSTOOL") == "1" {
			t.Fatalf("DEVSTRAP_REQUIRE_VCSTOOL=1 but vcs-import is not on PATH: %v", err)
		}
		t.Skip("vcstool not installed; TestGoldenStaysVCSToolCompatible is the always-on gate")
	}
	dir := t.TempDir()
	first := initBareRepo(t, filepath.Join(dir, "first.git"), "main")
	second := initBareRepo(t, filepath.Join(dir, "second.git"), "trunk")

	raw, err := Encode(Manifest{
		Repositories: map[string]RepoEntry{
			"work/first":  {Type: VCSTypeGit, URL: "file://" + first, Version: "main"},
			"oss/second":  {Type: VCSTypeGit, URL: "file://" + second, Version: "trunk"},
			"skip/nothin": {Type: VCSTypeGit, URL: "file://" + first},
		},
		DevStrap: Section{
			SchemaVersion: SchemaVersion, WorkspaceID: "ws_1", ExportedAt: "2026-08-01T00:00:00Z",
			Projects: map[string]Project{
				"work/first":    {Type: "git_repo", DefaultBranch: "main", EnvProfile: true},
				"oss/second":    {Type: "git_repo", DefaultBranch: "trunk"},
				"skip/nothin":   {Type: "git_repo"},
				"notes/scratch": {Type: "plain_folder"},
			},
		},
	})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	manifestFile := filepath.Join(dir, "workspace.yaml")
	if err := os.WriteFile(manifestFile, raw, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	target := filepath.Join(dir, "recovered")
	if err := os.MkdirAll(target, 0o750); err != nil {
		t.Fatalf("create target: %v", err)
	}
	cmd := exec.Command(vcs, "--input", manifestFile, target) //nolint:gosec // vcs path comes from exec.LookPath, args are test-owned temp paths.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("vcs import failed: %v\n%s", err, out)
	}
	for _, path := range []string{"work/first", "oss/second", "skip/nothin"} {
		if _, err := os.Stat(filepath.Join(target, path, ".git")); err != nil {
			t.Errorf("vcs import did not reconstruct %s: %v\n%s", path, err, out)
		}
	}
	// The honest half of the claim: a non-git project is structurally invisible
	// to vcstool. If this ever starts existing, the header's scoping is wrong.
	if _, err := os.Stat(filepath.Join(target, "notes/scratch")); err == nil {
		t.Error("vcs import created notes/scratch; the manifest header claims non-git projects are invisible to vcstool")
	}
}

func initBareRepo(t *testing.T, path, branch string) string {
	t.Helper()
	work := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...) //nolint:gosec // fixed git args in a test.
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=devstrap-test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=devstrap-test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(filepath.Dir(path), "init", "--bare", "-b", branch, path)
	run(filepath.Dir(work), "clone", path, work)
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("write README: %v", err)
	}
	run(work, "add", "README.md")
	run(work, "commit", "-m", "init")
	run(work, "push", "origin", branch)
	return path
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
