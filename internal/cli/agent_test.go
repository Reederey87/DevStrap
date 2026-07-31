package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/ignore"
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

// TestAgentSensitiveParityWithSandboxDenyList pins P7-SEC-01 (CodeRabbit
// parity): the wrapper-level file classifier must flag the same git/cloud
// credential files the OS sandbox masks, so a path that is kernel-denied is
// not simultaneously waved through by the coarse wrapper policy.
func TestAgentSensitiveParityWithSandboxDenyList(t *testing.T) {
	for _, f := range ignore.CredentialHomeFiles() {
		if !agentTokenLooksSensitive("/home/dev/" + f) {
			t.Errorf("agentTokenLooksSensitive(%q) = false, want true (sandbox masks it)", f)
		}
	}
	root := "/work/tree"
	for _, dir := range ignore.CredentialHomeDirs() {
		p := filepath.Join("/home/dev", dir, "credential")
		if !agentPathLooksSensitive(root, p) {
			t.Errorf("agentPathLooksSensitive(%q) = false, want true (outside worktree + credential path)", p)
		}
	}
}

func TestAgentAllowsCheckedInEnvExample(t *testing.T) {
	root := t.TempDir()
	args := []string{"head", ".env.example"}
	if err := enforceAgentCommandPolicy("guarded", args); err != nil {
		t.Fatalf("enforceAgentCommandPolicy(%v): %v", args, err)
	}
	if err := enforceAgentFilePolicy("guarded", args, root); err != nil {
		t.Fatalf("enforceAgentFilePolicy(%v): %v", args, err)
	}
}

func TestAgentDeniesDotKeyThatCanonicalDoesNot(t *testing.T) {
	const name = "en-US.key"
	if !agentTokenLooksSensitive(name) {
		t.Fatalf("agentTokenLooksSensitive(%q) = false, want true", name)
	}
	if ignore.IsSecretName(name) {
		t.Fatalf("ignore.IsSecretName(%q) = true, want false", name)
	}
}

// TestAgentStillDeniesRealEnvAfterDroppingTheSubstringPattern is the other half
// of the .env.example narrowing, and the half that is easy to forget: this PR
// DELETED the literal "cat .env" entry from enforceAgentPolicy's substring deny
// list (it matched `cat .env.example` via strings.Contains over the joined
// argv). Removing a guard is only safe if something else still catches the real
// case — here enforceAgentFilePolicy, via the canonical ignore.IsSecretName.
//
// Without this test, a later change could reopen the hole with every other test
// still green, because the only other .env assertion in the suite is that the
// EXAMPLE is allowed.
//
// Verified falsifiable by mutation, and the result is worth recording: breaking
// agentTokenLooksSensitive ALONE does not fail this test, because
// agentPathLooksSensitive's per-component check catches .env independently.
// Both had to be broken together to make it fail. That is genuine defense in
// depth rather than redundancy — but it also means this test pins the
// user-visible property ("a real .env is denied"), not either layer on its own.
func TestAgentStillDeniesRealEnvAfterDroppingTheSubstringPattern(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"bare .env", []string{"cat", ".env"}},
		{"nested .env", []string{"cat", "config/.env"}},
		{"dotted variant", []string{"cat", ".env.production"}},
		// The destination token is what blocks this one; the source is allowed.
		{"example copied onto .env", []string{"cp", ".env.example", ".env"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := enforceAgentFilePolicy("guarded", c.args, t.TempDir()); err == nil {
				t.Fatalf("enforceAgentFilePolicy(%v) allowed a real secret; the substring pattern was removed and nothing replaced it", c.args)
			}
		})
	}
}
