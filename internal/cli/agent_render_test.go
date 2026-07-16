package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
)

// P5-CLI-01 part B: agent run/pr and conflicts resolve --json shapes via the
// shared opts.render seam (agent list/show and conflicts list/show were part A).

func TestAgentRunJSON(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}

	tmp := t.TempDir()
	remote := filepath.Join(tmp, "repo.git")
	seed := filepath.Join(tmp, "seed")
	runGit(t, tmp, "init", "--bare", remote)
	runGit(t, seed, "init")
	runGit(t, seed, "config", "user.email", "devstrap@example.test")
	runGit(t, seed, "config", "user.name", "DevStrap Test")
	runGit(t, seed, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "add", "README.md")
	runGit(t, seed, "commit", "-m", "initial")
	runGit(t, seed, "remote", "add", "origin", remote)
	runGit(t, seed, "push", "origin", "main")
	runGit(t, tmp, "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/main")

	if _, stderr, err := executeForTest("--home", home, "add", "file://"+remote, "--path", "work/acme/agent-json-repo", "--default-branch", "main"); err != nil {
		t.Fatalf("add stderr = %q err = %v", stderr, err)
	}

	stdout, stderr, err := executeForTest("--home", home, "--json",
		"agent", "run", "work/acme/agent-json-repo",
		"--engine", "generic", "--task", "json run", "--sandbox", "off",
		"--", "touch", "agent-json.txt")
	if err != nil {
		t.Fatalf("agent run --json stderr = %q err = %v", stderr, err)
	}

	var got struct {
		state.AgentRun
		Worktree string `json:"worktree"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("agent run --json is not AgentRun+worktree: %v\n%s", err, stdout)
	}
	if got.ID == "" || got.Status != "complete" {
		t.Fatalf("agent run --json = %+v, want complete run with id", got)
	}
	if got.Worktree == "" {
		t.Fatalf("agent run --json missing worktree: %+v", got)
	}
	if !strings.Contains(got.DiffSummary, "agent-json.txt") {
		t.Fatalf("agent run --json DiffSummary = %q, want agent-json.txt", got.DiffSummary)
	}
	if got.TestSummary != "command exited 0" {
		t.Fatalf("agent run --json TestSummary = %q, want command exited 0", got.TestSummary)
	}
	// Stale-status regression: payload must not still say "running".
	if got.Status == "running" {
		t.Fatalf("agent run --json Status still running (stale INSERT-time copy)")
	}
}

func TestAgentPRJSON(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}

	tmp := t.TempDir()
	remote := filepath.Join(tmp, "repo.git")
	seed := filepath.Join(tmp, "seed")
	runGit(t, tmp, "init", "--bare", remote)
	runGit(t, seed, "init")
	runGit(t, seed, "config", "user.email", "devstrap@example.test")
	runGit(t, seed, "config", "user.name", "DevStrap Test")
	runGit(t, seed, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "add", "README.md")
	runGit(t, seed, "commit", "-m", "initial")
	runGit(t, seed, "remote", "add", "origin", remote)
	runGit(t, seed, "push", "origin", "main")
	runGit(t, tmp, "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/main")

	if _, stderr, err := executeForTest("--home", home, "add", "file://"+remote, "--path", "work/acme/agent-pr-json", "--default-branch", "main"); err != nil {
		t.Fatalf("add stderr = %q err = %v", stderr, err)
	}
	if _, stderr, err := executeForTest("--home", home,
		"agent", "run", "work/acme/agent-pr-json",
		"--engine", "generic", "--task", "pr json", "--sandbox", "off",
		"--", "touch", "pr.txt"); err != nil {
		t.Fatalf("agent run stderr = %q err = %v", stderr, err)
	}

	listOut, _, err := executeForTest("--home", home, "--json", "agent", "list")
	if err != nil {
		t.Fatalf("agent list: %v", err)
	}
	var runs []state.AgentRun
	if err := json.Unmarshal([]byte(listOut), &runs); err != nil || len(runs) != 1 {
		t.Fatalf("agent list = %q err=%v, want one run", listOut, err)
	}
	runID := runs[0].ID

	stdout, stderr, err := executeForTest("--home", home, "--json",
		"agent", "pr", runID, "--dry-run", "--allow-stale-base")
	if err != nil {
		t.Fatalf("agent pr --json --dry-run stderr = %q err = %v", stderr, err)
	}
	var got agentPRResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("agent pr --json is not agentPRResult: %v\n%s", err, stdout)
	}
	if got.RunID != runID || !got.DryRun {
		t.Fatalf("agent pr --json = %+v, want run_id=%s dry_run=true", got, runID)
	}
	if got.Base == "" || got.Head == "" {
		t.Fatalf("agent pr --json = %+v, want base and head", got)
	}
	if got.URL != "" {
		t.Fatalf("agent pr --json dry-run has url=%q, want empty", got.URL)
	}
}

func TestConflictsResolveJSON(t *testing.T) {
	ctx := context.Background()
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertConflict(ctx, "", "same-path/different-remote", `{"path":"work/acme/api"}`); err != nil {
		t.Fatal(err)
	}
	open, err := store.OpenConflicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("open conflicts = %d, want 1", len(open))
	}
	conflictID := open[0].ID
	_ = store.Close()

	stdout, stderr, err := executeForTest("--home", home, "--json",
		"conflicts", "resolve", conflictID, "--keep-local")
	if err != nil {
		t.Fatalf("conflicts resolve --json stderr = %q err = %v", stderr, err)
	}
	var got conflictResolveResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("conflicts resolve --json is not conflictResolveResult: %v\n%s", err, stdout)
	}
	if got.ConflictID != conflictID || got.Action != "keep-local" {
		t.Fatalf("conflicts resolve --json = %+v, want conflict_id=%s action=keep-local", got, conflictID)
	}
}
