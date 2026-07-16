package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/platform"
)

// P5-CLI-01 part B (final): --json coverage for the remaining leaf commands —
// init, add, clone, hydrate, open, version, service install/uninstall, up,
// join. `run` and `pair` are documented as intentionally exempt (see
// spec/13_CLI_DAEMON_API.md) and are not covered here.

func TestVersionJSON(t *testing.T) {
	stdout, stderr, err := executeForTest("--json", "version")
	if err != nil {
		t.Fatalf("version --json: %v\nstderr=%s", err, stderr)
	}
	var got versionResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("version --json is not versionResult: %v\n%s", err, stdout)
	}
	if got.Version == "" || got.Commit == "" || got.Date == "" {
		t.Fatalf("got = %+v, want all fields populated", got)
	}
}

func TestInitJSONStandalone(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json", "init")
	if err != nil {
		t.Fatalf("init --json: %v\nstderr=%s", err, stderr)
	}
	var got initResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("init --json is not initResult: %v\n%s", err, stdout)
	}
	if got.Root != root {
		t.Errorf("root = %q, want %q", got.Root, root)
	}
	if got.WorkspaceName != "default" {
		t.Errorf("workspace_name = %q, want default", got.WorkspaceName)
	}
	if got.DryRun || got.Join {
		t.Errorf("got = %+v, want dry_run/join both false", got)
	}
}

func TestInitJSONDryRun(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json", "init", "--dry-run")
	if err != nil {
		t.Fatalf("init --dry-run --json: %v\nstderr=%s", err, stderr)
	}
	var got initResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("init --dry-run --json is not initResult: %v\n%s", err, stdout)
	}
	if !got.DryRun {
		t.Errorf("got = %+v, want dry_run true", got)
	}
	if got.Root != root || got.Home != home {
		t.Errorf("got = %+v, want root %q home %q", got, root, home)
	}
}

func TestAddJSON(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init: %v\nstderr=%s", err, stderr)
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	mustGit(t, "", "init", "--bare", "-b", "main", remote)

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json", "add", remote, "--path", "work/proj")
	if err != nil {
		t.Fatalf("add --json: %v\nstderr=%s", err, stderr)
	}
	var got addResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("add --json is not addResult: %v\n%s", err, stdout)
	}
	if got.Path != "work/proj" || got.Remote != remote {
		t.Errorf("got = %+v, want path work/proj remote %q", got, remote)
	}
}

func TestCloneJSON(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init: %v\nstderr=%s", err, stderr)
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	mustGit(t, "", "init", "--bare", "-b", "main", remote)
	seedRemoteWithCommit(t, remote)

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json", "clone", remote)
	if err != nil {
		t.Fatalf("clone --json: %v\nstderr=%s", err, stderr)
	}
	var got cloneResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("clone --json is not cloneResult: %v\n%s", err, stdout)
	}
	if got.Remote != remote {
		t.Errorf("remote = %q, want %q", got.Remote, remote)
	}
	if got.Editor != "" {
		t.Errorf("editor = %q, want empty (no --open passed)", got.Editor)
	}
	if !strings.Contains(got.OpenHint, "devstrap open "+got.Path) {
		t.Errorf("open_hint = %q, want it to reference %q", got.OpenHint, got.Path)
	}
}

func TestHydrateJSON(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init: %v\nstderr=%s", err, stderr)
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	mustGit(t, "", "init", "--bare", "-b", "main", remote)
	seedRemoteWithCommit(t, remote)
	if _, stderr, err := executeForTest("--home", home, "--root", root, "add", remote, "--path", "work/proj"); err != nil {
		t.Fatalf("add: %v\nstderr=%s", err, stderr)
	}

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json", "hydrate", "work/proj")
	if err != nil {
		t.Fatalf("hydrate --json: %v\nstderr=%s", err, stderr)
	}
	var got hydrateResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("hydrate --json is not hydrateResult: %v\n%s", err, stdout)
	}
	wantPath := filepath.Join(root, "work", "proj")
	if got.Path != wantPath {
		t.Errorf("path = %q, want %q", got.Path, wantPath)
	}
}

func TestOpenJSON(t *testing.T) {
	if platform.Detect().OS == "windows" {
		t.Skip("fake-editor-on-PATH trick assumes a unix shebang script")
	}
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init: %v\nstderr=%s", err, stderr)
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	mustGit(t, "", "init", "--bare", "-b", "main", remote)
	seedRemoteWithCommit(t, remote)
	if _, stderr, err := executeForTest("--home", home, "--root", root, "add", remote, "--path", "work/proj"); err != nil {
		t.Fatalf("add: %v\nstderr=%s", err, stderr)
	}
	installFakeEditor(t, "cursor")

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json", "open", "work/proj", "--cursor")
	if err != nil {
		t.Fatalf("open --json: %v\nstderr=%s", err, stderr)
	}
	var got openResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("open --json is not openResult: %v\n%s", err, stdout)
	}
	if got.Editor != "cursor" {
		t.Errorf("editor = %q, want cursor", got.Editor)
	}
	wantPath := filepath.Join(root, "work", "proj")
	if got.Path != wantPath {
		t.Errorf("path = %q, want %q", got.Path, wantPath)
	}
}

func TestServiceInstallUninstallJSON(t *testing.T) {
	f := &fakeServiceManager{statusVal: platform.ServiceStatus{UnitPath: "/fake/unit.plist"}}
	withFakeService(t, f)
	home := t.TempDir()
	hub := filepath.Join(t.TempDir(), "hub.json")

	stdout, stderr, err := executeForTest("--home", home, "--json", "service", "install", "--hub-file", hub, "--exec-path", "/usr/local/bin/devstrap")
	if err != nil {
		t.Fatalf("service install --json: %v\nstderr=%s", err, stderr)
	}
	var install serviceInstallResult
	if err := json.Unmarshal([]byte(stdout), &install); err != nil {
		t.Fatalf("service install --json is not serviceInstallResult: %v\n%s", err, stdout)
	}
	if install.Manager != f.Name() || install.UnitPath != f.statusVal.UnitPath {
		t.Errorf("got = %+v, want manager %q unit_path %q", install, f.Name(), f.statusVal.UnitPath)
	}
	// The stderr confirmation (P7-CLI-03) must survive unchanged alongside --json.
	if !strings.Contains(stderr, "installed") {
		t.Errorf("stderr = %q, want the stderr confirmation preserved", stderr)
	}

	f2 := &fakeServiceManager{statusVal: platform.ServiceStatus{Installed: true}}
	withFakeService(t, f2)
	stdout2, stderr2, err := executeForTest("--home", home, "--json", "service", "uninstall")
	if err != nil {
		t.Fatalf("service uninstall --json: %v\nstderr=%s", err, stderr2)
	}
	var uninstall serviceUninstallResult
	if err := json.Unmarshal([]byte(stdout2), &uninstall); err != nil {
		t.Fatalf("service uninstall --json is not serviceUninstallResult: %v\n%s", err, stdout2)
	}
	if !uninstall.WasInstalled {
		t.Errorf("got = %+v, want was_installed true", uninstall)
	}
}

// TestUpJSONInheritsSyncResult is the wave's critical regression guard for the
// init/up/join nested-render design: `up --json` must produce EXACTLY ONE JSON
// document on stdout, and it must be `syncResult` (from up's terminal
// runSyncCycle call) — NOT initResult, and NOT two concatenated documents.
// This guards against runInit's internal render firing when calledFromUp is
// set, which would otherwise corrupt --json output with a second document.
func TestUpJSONInheritsSyncResult(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	hubPath := filepath.Join(t.TempDir(), "hub.json")
	hubURI := "file:" + hubPath

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json", "up", "--hub", hubURI, "--scan=false")
	if err != nil {
		t.Fatalf("up --json: %v\nstderr=%s", err, stderr)
	}

	dec := json.NewDecoder(strings.NewReader(stdout))
	var docs []map[string]any
	for {
		var doc map[string]any
		derr := dec.Decode(&doc)
		if derr == io.EOF {
			break
		}
		if derr != nil {
			t.Fatalf("stdout is not a clean single JSON document: %v\nstdout=%s", derr, stdout)
		}
		docs = append(docs, doc)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d JSON documents on stdout, want exactly 1:\n%s", len(docs), stdout)
	}
	doc := docs[0]
	if _, ok := doc["hub_id"]; !ok {
		t.Errorf("document = %v, want a syncResult (hub_id key present)", doc)
	}
	if _, ok := doc["workspace_name"]; ok {
		t.Errorf("document = %v, want NOT an initResult (workspace_name key must be absent)", doc)
	}
	if _, ok := doc["adopted"]; ok {
		t.Errorf("document = %v, want NOT an initResult (adopted key must be absent)", doc)
	}
}

func TestJoinJSON(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	// Founder side: found a workspace and mint this device's pairing code.
	founderHome := filepath.Join(t.TempDir(), ".devstrap")
	founderRoot := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", founderHome, "--root", founderRoot, "init"); err != nil {
		t.Fatalf("founder init: %v\nstderr=%s", err, stderr)
	}
	code, stderr, err := executeForTest("--home", founderHome, "devices", "pairing-code")
	if err != nil {
		t.Fatalf("devices pairing-code: %v\nstderr=%s", err, stderr)
	}
	code = strings.TrimSpace(code)

	joinerHome := filepath.Join(t.TempDir(), ".devstrap")
	joinerRoot := filepath.Join(t.TempDir(), "Code")
	stdout, stderr, err := executeForTest("--home", joinerHome, "--root", joinerRoot, "--json", "join", code)
	if err != nil {
		t.Fatalf("join --json: %v\nstderr=%s", err, stderr)
	}
	var got joinResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("join --json is not joinResult: %v\n%s", err, stdout)
	}
	if got.Code == "" || got.Fingerprint == "" || got.WorkspaceID == "" {
		t.Fatalf("got = %+v, want code/fingerprint/workspace_id populated", got)
	}
}

// seedRemoteWithCommit pushes one commit to a bare remote so hydrate/clone have
// something real to clone.
func seedRemoteWithCommit(t *testing.T, remote string) {
	t.Helper()
	staging := t.TempDir()
	mustGit(t, staging, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(staging, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, staging, "add", "README.md")
	mustGit(t, staging, "commit", "-m", "init")
	mustGit(t, staging, "remote", "add", "origin", remote)
	mustGit(t, staging, "push", "origin", "main")
}

// installFakeEditor writes an executable no-op script named `name` into a temp
// dir prepended to PATH, so `open`/`clone --open` can exercise the real
// platform.SystemEditor.Open success path without launching a real GUI editor.
func installFakeEditor(t *testing.T, name string) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, name)
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // test fixture, not user input
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
