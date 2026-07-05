package specdrift

import (
	"bytes"
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

func TestReleaseTierFilesRequireWorkLog(t *testing.T) {
	// P4-PROD-05 follow-through: the release/distribution tier
	// (.goreleaser.yaml, scripts/**) must be work-log-gated like the rest of
	// the shipping surface — a lone packaging change is still a behavior change.
	root := t.TempDir()
	writeSpec(t, root, "03_SYSTEM_ARCHITECTURE.md", "[.goreleaser.yaml, scripts/**]")
	writeSpec(t, root, "18_WORK_LOG.md", "[**]")

	for _, file := range []string{".goreleaser.yaml", "scripts/install.sh"} {
		report, err := Check(context.Background(), Options{
			RepoRoot:       root,
			ChangedFiles:   []string{file},
			RequireWorkLog: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(report.Findings, "\n")
		if !strings.Contains(joined, "spec/18_WORK_LOG.md was not updated") {
			t.Fatalf("changing %s alone: findings = %v, want work-log failure", file, report.Findings)
		}
	}

	report, err := Check(context.Background(), Options{
		RepoRoot:       root,
		ChangedFiles:   []string{".goreleaser.yaml", "scripts/install.sh", "spec/03_SYSTEM_ARCHITECTURE.md", "spec/18_WORK_LOG.md"},
		RequireWorkLog: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		t.Fatalf("findings = %v, want none when the tracking spec and work log change too", report.Findings)
	}
}

func TestEveryInternalPackageHasASpecificSpecOwner(t *testing.T) {
	root := filepath.Join("..", "..")
	specs, findings, err := LoadSpecs(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) > 0 {
		t.Fatalf("spec findings = %v", findings)
	}

	entries, err := os.ReadDir(filepath.Join(root, "internal"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pkg := entry.Name()
		specific, _ := specsTrackingTiers(specs, "internal/"+pkg+"/x.go")
		if len(specific) == 0 {
			t.Errorf("internal/%s has no specific spec owner (only broad/catch-all matches)", pkg)
		}
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

func TestAdvisoryModeExitsCleanWithWarnings(t *testing.T) {
	// AD-8: fork PRs get advisory mode — findings surface as GitHub Actions
	// warning annotations but never fail the job.
	report := Report{Findings: []string{"finding one", "finding two"}}
	var stdout, stderr bytes.Buffer

	if exitNonZero := PrintReport(&stdout, &stderr, report, true); exitNonZero {
		t.Fatal("advisory mode must not request a non-zero exit")
	}
	out := stdout.String()
	for _, finding := range report.Findings {
		want := "::warning::spec-drift (advisory on fork PRs): " + finding
		if !strings.Contains(out, want) {
			t.Fatalf("stdout = %q, want warning annotation %q", out, want)
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("advisory mode wrote to stderr: %q", stderr.String())
	}
	if !strings.Contains(out, "spec drift check found issues (advisory on fork PRs, not blocking):\n- finding one\n- finding two\n") {
		t.Fatalf("stdout = %q, want the human-readable advisory footer", out)
	}
}

func TestPrintReportPassingSummaryBothModes(t *testing.T) {
	// A passing report behaves identically in both modes: the one-line
	// summary on stdout, nothing on stderr, exit 0.
	report := Report{Specs: make([]Spec, 3), ChangedFiles: []string{"a", "b"}}
	for _, advisory := range []bool{false, true} {
		var stdout, stderr bytes.Buffer
		if exitNonZero := PrintReport(&stdout, &stderr, report, advisory); exitNonZero {
			t.Fatalf("advisory=%v: passing report must not request a non-zero exit", advisory)
		}
		if got, want := stdout.String(), "spec drift check passed: 3 specs, 2 changed files\n"; got != want {
			t.Fatalf("advisory=%v: stdout = %q, want %q", advisory, got, want)
		}
		if stderr.Len() != 0 {
			t.Fatalf("advisory=%v: passing report wrote to stderr: %q", advisory, stderr.String())
		}
	}
}

func TestStrictModeUnchanged(t *testing.T) {
	// Same finding set as TestAdvisoryModeExitsCleanWithWarnings, but strict
	// mode must still fail with the pre-advisory message text and exit
	// request.
	report := Report{Findings: []string{"finding one"}}
	var stdout, stderr bytes.Buffer

	if exitNonZero := PrintReport(&stdout, &stderr, report, false); !exitNonZero {
		t.Fatal("strict mode with findings must request a non-zero exit")
	}
	if stdout.Len() != 0 {
		t.Fatalf("strict mode wrote to stdout: %q", stdout.String())
	}
	want := "spec drift check failed:\n- finding one\n"
	if stderr.String() != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
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
