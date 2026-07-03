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
	if !strings.Contains(joined, "internal/cli/root.go changed but none of the required specific specs changed") {
		t.Fatalf("findings = %v, want specific mapped spec failure", report.Findings)
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

func TestMappedSpecNotSatisfiedByWorkLogCatchAll(t *testing.T) {
	root := t.TempDir()
	writeSpec(t, root, "13_CLI_DAEMON_API.md", "[internal/cli/**]")
	writeSpec(t, root, "18_WORK_LOG.md", "[**]")

	report, err := Check(context.Background(), Options{
		RepoRoot:       root,
		ChangedFiles:   []string{"internal/cli/root.go", "spec/18_WORK_LOG.md"},
		RequireWorkLog: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.OK() {
		t.Fatal("expected mapped-spec drift for internal/cli to fail despite work-log catch-all")
	}
	joined := strings.Join(report.Findings, "\n")
	if !strings.Contains(joined, "internal/cli/root.go") || !strings.Contains(joined, "spec/13_CLI_DAEMON_API.md") {
		t.Fatalf("findings = %v, want changed file and required specific spec named", report.Findings)
	}
}

func TestMappedSpecSatisfiedBySpecificSpec(t *testing.T) {
	root := t.TempDir()
	writeSpec(t, root, "13_CLI_DAEMON_API.md", "[internal/cli/**]")
	writeSpec(t, root, "18_WORK_LOG.md", "[**]")

	report, err := Check(context.Background(), Options{
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

func TestBroadSpecDoesNotSatisfyWhenSpecificExists(t *testing.T) {
	root := t.TempDir()
	writeSpec(t, root, "00_START_HERE.md", "[internal/**]")
	writeSpec(t, root, "13_CLI_DAEMON_API.md", "[internal/cli/**]")
	writeSpec(t, root, "18_WORK_LOG.md", "[**]")

	report, err := Check(context.Background(), Options{
		RepoRoot:       root,
		ChangedFiles:   []string{"internal/cli/root.go", "spec/00_START_HERE.md", "spec/18_WORK_LOG.md"},
		RequireWorkLog: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.OK() {
		t.Fatal("expected broad spec not to satisfy a file with a specific owner")
	}
	joined := strings.Join(report.Findings, "\n")
	if !strings.Contains(joined, "spec/13_CLI_DAEMON_API.md") || strings.Contains(joined, "spec/00_START_HERE.md") {
		t.Fatalf("findings = %v, want only the specific owner listed", report.Findings)
	}
}

func TestBroadOnlyFileSatisfiedByBroadSpec(t *testing.T) {
	root := t.TempDir()
	writeSpec(t, root, "16_TEST_PLAN.md", "[internal/**]")
	writeSpec(t, root, "18_WORK_LOG.md", "[**]")

	report, err := Check(context.Background(), Options{
		RepoRoot:       root,
		ChangedFiles:   []string{"internal/specdrift/specdrift.go", "spec/16_TEST_PLAN.md", "spec/18_WORK_LOG.md"},
		RequireWorkLog: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		t.Fatalf("findings = %v, want broad-only mapping satisfied by broad spec", report.Findings)
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
