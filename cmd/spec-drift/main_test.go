package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=spec-drift-test",
		"GIT_AUTHOR_EMAIL=spec-drift-test@example.com",
		"GIT_COMMITTER_NAME=spec-drift-test",
		"GIT_COMMITTER_EMAIL=spec-drift-test@example.com",
		// Isolate from the developer's global/system git config (e.g.
		// commit.gpgsign, core.hooksPath), matching internal/git/git_test.go.
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// newCleanRepo creates a git repo with one committed spec directory
// (a catch-all "[**]" work-log spec, matching internal/specdrift's own
// convention) and nothing else changed, so a run against it reports no
// drift.
func newCleanRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet", "--initial-branch=main")
	specDir := filepath.Join(dir, "spec")
	if err := os.MkdirAll(specDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSpecFile(t, specDir, "18_WORK_LOG.md", "[**]")
	// A placeholder tracked file under internal/ so that a later untracked
	// internal/foo.go is reported by `git status` as its own path rather
	// than collapsed into a single "internal/" untracked-directory line.
	internalDir := filepath.Join(dir, "internal")
	if err := os.MkdirAll(internalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(internalDir, "placeholder.go"), []byte("package internal\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "--quiet", "-m", "initial")
	return dir
}

func writeSpecFile(t *testing.T, specDir, name, tracksInline string) {
	t.Helper()
	content := "---\nlast_reviewed: 2026-07-01\ntracks_code: " + tracksInline + "\n---\n# " + name + "\n"
	if err := os.WriteFile(filepath.Join(specDir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunInvalidFlagReturnsExitCode2(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--bogus-flag"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
}

// TestRunHelpFlagExitsZero pins the flag.ErrHelp carve-out: stdlib
// flag.CommandLine (ExitOnError) exits 0 on -h/--help, unlike every other
// parse error. The FlagSet-based run() must preserve that distinction rather
// than collapsing help into the generic exit-2 usage-error path.
func TestRunHelpFlagExitsZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-h"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr.String())
	}
}

func TestRunCleanRepoExitsZero(t *testing.T) {
	dir := newCleanRepo(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--repo", dir, "--base", "HEAD", "--head", "HEAD"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "spec drift check passed") {
		t.Fatalf("stdout = %q, want pass summary", stdout.String())
	}
}

func TestRunStrictModeFailsOnDrift(t *testing.T) {
	dir := newCleanRepo(t)
	if err := os.MkdirAll(filepath.Join(dir, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "internal", "foo.go"), []byte("package internal\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"--repo", dir, "--base", "HEAD", "--head", "HEAD"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "spec drift check failed:") {
		t.Fatalf("stderr = %q, want failure header", stderr.String())
	}
	if !strings.Contains(stderr.String(), "spec/18_WORK_LOG.md was not updated") {
		t.Fatalf("stderr = %q, want work log finding", stderr.String())
	}
}

func TestRunAdvisoryModeExitsCleanWithWarnings(t *testing.T) {
	dir := newCleanRepo(t)
	if err := os.MkdirAll(filepath.Join(dir, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "internal", "foo.go"), []byte("package internal\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"--repo", dir, "--base", "HEAD", "--head", "HEAD", "--advisory"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0 (advisory never fails); stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "::warning::") {
		t.Fatalf("stdout = %q, want a GitHub Actions warning annotation", stdout.String())
	}
	if !strings.Contains(stdout.String(), "advisory on fork PRs") {
		t.Fatalf("stdout = %q, want advisory framing", stdout.String())
	}
}

func TestRunCheckErrorNonGitRepo(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run([]string{"--repo", dir, "--base", "HEAD", "--head", "HEAD"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "spec drift check failed: git diff changed files") {
		t.Fatalf("stderr = %q, want a wrapped git diff error", stderr.String())
	}
}
