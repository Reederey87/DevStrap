package specdrift

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckRequiresMappedSpecAndWorkLog(t *testing.T) {
	root := t.TempDir()
	writeSpec(t, root, "00_START_HERE.md", "[cmd/**, internal/cli/**]")
	writeSpec(t, root, "13_CLI_DAEMON_API.md", "[internal/cli/**]")
	writeSpec(t, root, "18_WORK_LOG.md", "[**]")

	report, err := Check(context.Background(), Options{
		RepoRoot:       root,
		ChangedFiles:   []string{"internal/cli/root.go"},
		RequireWorkLog: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.OK() {
		t.Fatal("expected drift findings")
	}
	joined := strings.Join(report.Findings, "\n")
	if !strings.Contains(joined, "internal/cli/root.go changed but none of the mapped specs changed") {
		t.Fatalf("findings = %v, want mapped spec failure", report.Findings)
	}
	if !strings.Contains(joined, "spec/18_WORK_LOG.md was not updated") {
		t.Fatalf("findings = %v, want work log failure", report.Findings)
	}

	report, err = Check(context.Background(), Options{
		RepoRoot:       root,
		ChangedFiles:   []string{"internal/cli/root.go", "spec/13_CLI_DAEMON_API.md", "spec/18_WORK_LOG.md"},
		RequireWorkLog: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		t.Fatalf("findings = %v, want none", report.Findings)
	}
}

func TestLoadSpecsValidatesFrontmatter(t *testing.T) {
	root := t.TempDir()
	specDir := filepath.Join(root, "spec")
	if err := os.MkdirAll(specDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(specDir, "00_START_HERE.md"), []byte("# Missing frontmatter\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, findings, err := LoadSpecs(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || !strings.Contains(findings[0], "missing YAML frontmatter") {
		t.Fatalf("findings = %v, want missing frontmatter", findings)
	}
}

func TestLoadSpecsRequiresClosingFrontmatterDelimiter(t *testing.T) {
	root := t.TempDir()
	specDir := filepath.Join(root, "spec")
	if err := os.MkdirAll(specDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nlast_reviewed: 2026-06-25\ntracks_code: [cmd/**]\n# Missing close\n"
	if err := os.WriteFile(filepath.Join(specDir, "00_START_HERE.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	_, findings, err := LoadSpecs(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || !strings.Contains(findings[0], "missing closing YAML frontmatter delimiter") {
		t.Fatalf("findings = %v, want missing closing delimiter", findings)
	}
}

func writeSpec(t *testing.T, root, name, tracks string) {
	t.Helper()
	specDir := filepath.Join(root, "spec")
	if err := os.MkdirAll(specDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nlast_reviewed: 2026-06-25\ntracks_code: " + tracks + "\n---\n# " + name + "\n"
	if err := os.WriteFile(filepath.Join(specDir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
