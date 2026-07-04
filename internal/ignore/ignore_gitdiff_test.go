package ignore

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestMatcherMatchesGitCheckIgnore(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}

	repo := t.TempDir()
	cmd := exec.Command(gitPath, "init")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	contents := "docs/build/\n" +
		"data/raw/\n" +
		"[!a]log\n" +
		"a**b\n" +
		"build/\n" +
		"!build/keep/\n" +
		"logs/*.log\n" +
		"x[!a]y\n" + // negated class must NOT match '/' (fnmatch FNM_PATHNAME)
		"a/***/b\n" // "***" is a regular star run, not the recursive "**"
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(contents), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}

	tests := []struct {
		rel   string
		isDir bool
	}{
		{rel: "docs/build", isDir: true},
		{rel: "packages/site/docs/build", isDir: true},
		{rel: "data/raw", isDir: true},
		{rel: "experiments/data/raw", isDir: true},
		{rel: "blog"},
		{rel: "alog"},
		{rel: "!log"},
		{rel: "axb"},
		{rel: "a/x/b"},
		{rel: "build", isDir: true},
		{rel: "build/tmp.txt"},
		{rel: "logs/app.log"},
		{rel: "src/logs/app.log"},
		{rel: "logs/app.txt"},
		{rel: "xzy"},     // x[!a]y matches (z is not 'a', not '/')
		{rel: "xay"},     // x[!a]y does not match (a excluded)
		{rel: "x/y"},     // x[!a]y must NOT match across '/'
		{rel: "a/m/b"},   // a/***/b matches one segment
		{rel: "a/m/n/b"}, // a/***/b is NOT recursive → no match
	}
	for _, tc := range tests {
		createCandidate(t, repo, tc.rel, tc.isDir)
	}

	m, err := Compile(contents, false)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	for _, tc := range tests {
		gitIgnored := gitCheckIgnore(t, gitPath, repo, tc.rel)
		got := m.Match(tc.rel, tc.isDir)
		if got != gitIgnored {
			t.Errorf("Match(%q, isDir=%v) = %v, git check-ignore = %v", tc.rel, tc.isDir, got, gitIgnored)
		}
	}
}

func TestCompileDoesNotFailOnUnclosedBracket(t *testing.T) {
	m, err := Compile("foo[1.txt\n", false)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !m.Match("foo[1.txt", false) {
		t.Fatal("unclosed bracket pattern did not match literal bracket path")
	}
}

func TestAnchoredMiddleSlashDoesNotMatchNested(t *testing.T) {
	m, err := Compile("docs/build/\n", false)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !m.Match("docs/build", true) {
		t.Fatal("docs/build/ did not match root docs/build directory")
	}
	if m.Match("pkg/docs/build", true) {
		t.Fatal("docs/build/ matched nested pkg/docs/build directory")
	}
}

func createCandidate(t *testing.T, repo, rel string, isDir bool) {
	t.Helper()
	path := filepath.Join(repo, filepath.FromSlash(rel))
	if isDir {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir parent for %s: %v", rel, err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func gitCheckIgnore(t *testing.T, gitPath, repo, rel string) bool {
	t.Helper()
	cmd := exec.Command(gitPath, "check-ignore", "--verbose", "--", rel)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false
	}
	t.Fatalf("git check-ignore %q: %v\n%s", rel, err, out)
	return false
}
