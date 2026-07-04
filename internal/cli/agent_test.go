package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentDiffSummaryCommittedVsBase(t *testing.T) {
	repo, baseSHA := setupAgentDiffRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "committed.txt"), []byte("committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "committed.txt")
	runGit(t, repo, "commit", "-m", "agent change")

	summary := agentDiffSummary(context.Background(), repo, baseSHA)
	if !strings.Contains(summary, "Committed since base:") {
		t.Fatalf("diff summary = %q, want committed section", summary)
	}
	if !strings.Contains(summary, "committed.txt") {
		t.Fatalf("diff summary = %q, want committed file", summary)
	}
	if !strings.Contains(summary, "Uncommitted:") {
		t.Fatalf("diff summary = %q, want uncommitted section", summary)
	}
}

func TestAgentDiffSummaryUncommittedResidue(t *testing.T) {
	repo, baseSHA := setupAgentDiffRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "dirty.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	summary := agentDiffSummary(context.Background(), repo, baseSHA)
	if !strings.Contains(summary, "Committed since base:\n(no changes)") {
		t.Fatalf("diff summary = %q, want no committed changes", summary)
	}
	if !strings.Contains(summary, "Uncommitted:") {
		t.Fatalf("diff summary = %q, want uncommitted section", summary)
	}
	if !strings.Contains(summary, "dirty.txt") {
		t.Fatalf("diff summary = %q, want dirty file", summary)
	}
}

func TestAgentDiffSummaryUnbornHead(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")

	summary := agentDiffSummary(context.Background(), repo, "")
	if strings.TrimSpace(summary) != "" {
		t.Fatalf("diff summary = %q, want empty summary for clean unborn HEAD", summary)
	}
}

func setupAgentDiffRepo(t *testing.T) (string, string) {
	t.Helper()
	repo := t.TempDir()
	runGit(t, repo, "init")
	runGit(t, repo, "config", "user.email", "devstrap@example.test")
	runGit(t, repo, "config", "user.name", "DevStrap Test")
	runGit(t, repo, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-m", "initial")
	return repo, strings.TrimSpace(runGitOutput(t, repo, "rev-parse", "HEAD"))
}
