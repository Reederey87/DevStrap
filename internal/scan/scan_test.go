package scan

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
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

func TestIsSecretName(t *testing.T) {
	cases := []struct {
		name, rel string
		want      bool
	}{
		{".env", "work/api/.env", true},
		{".env.production", "work/api/.env.production", true},
		{".env.local", "work/api/.env.local", true},
		{".env.example", "work/api/.env.example", false},
		{".env.template", "work/api/.env.template", false},
		{".env.schema", "work/api/.env.schema", false},
		{"key.pem", "work/api/key.pem", true},
		{"id_rsa", "work/api/id_rsa", true},
		{"credentials.json", "work/api/credentials.json", true},
		{"credentials", "work/api/.aws/credentials", true},
		{"config.toml", "work/api/.snowflake/config.toml", true},
		{"README.md", "work/api/README.md", false},
		{"main.go", "work/api/main.go", false},
	}
	for _, c := range cases {
		t.Run(c.name+"|"+c.rel, func(t *testing.T) {
			if got := isSecretName(c.name, c.rel); got != c.want {
				t.Fatalf("isSecretName(%q,%q)=%v want %v", c.name, c.rel, got, c.want)
			}
		})
	}
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
			if got := shouldPruneDir(c.name, c.rel); got != c.want {
				t.Fatalf("shouldPruneDir(%q,%q)=%v want %v", c.name, c.rel, got, c.want)
			}
		})
	}
	// rel-suffix data dirs.
	if !shouldPruneDir("raw", "work/ml/data/raw") {
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

func hasWarning(warnings []string, needle string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning, needle) {
			return true
		}
	}
	return false
}
