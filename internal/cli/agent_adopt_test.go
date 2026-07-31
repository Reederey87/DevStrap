package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
)

// agentAdoptResultForTest mirrors agentAdoptResult's --json shape for
// decoding in tests.
type agentAdoptResultForTest struct {
	state.AgentRun
	SchemaVersion int      `json:"schema_version"`
	WorktreePath  string   `json:"worktree_path"`
	Warnings      []string `json:"warnings,omitempty"`
}

// agentFinishResultForTest mirrors agentFinishResult's --json shape.
type agentFinishResultForTest struct {
	state.AgentRun
	SchemaVersion int      `json:"schema_version"`
	Warnings      []string `json:"warnings,omitempty"`
}

func TestAgentAdoptRequiresEngineAndTask(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	localPath := setupFreshWorktreeRepo(t, home, root, "auto", false)
	head := strings.TrimSpace(runGitOutput(t, localPath, "rev-parse", "HEAD"))
	extWT := filepath.Join(t.TempDir(), "external-wt")
	runGit(t, localPath, "worktree", "add", "--detach", extWT, head)

	_, stderr, err := executeForTest("--home", home, "--root", root, "agent", "adopt", extWT, "--task", "fix tests")
	if err == nil {
		t.Fatal("want an error for a missing --engine")
	}
	if !strings.Contains(stderr, "--engine is required") {
		t.Fatalf("stderr = %q, want --engine required refusal", stderr)
	}

	_, stderr, err = executeForTest("--home", home, "--root", root, "agent", "adopt", extWT, "--engine", "claude-code")
	if err == nil {
		t.Fatal("want an error for a missing --task")
	}
	if !strings.Contains(stderr, "--task is required") {
		t.Fatalf("stderr = %q, want --task required refusal", stderr)
	}
}

// TestAgentAdoptRefusesUnknownWorktreeWithoutAdoptFlag proves an
// externally-created but never-registered worktree is refused with the
// --adopt-worktree remedy rather than silently registered.
func TestAgentAdoptRefusesUnknownWorktreeWithoutAdoptFlag(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	localPath := setupFreshWorktreeRepo(t, home, root, "auto", false)
	head := strings.TrimSpace(runGitOutput(t, localPath, "rev-parse", "HEAD"))
	extWT := filepath.Join(t.TempDir(), "external-wt")
	runGit(t, localPath, "worktree", "add", "--detach", extWT, head)

	_, stderr, err := executeForTest("--home", home, "--root", root, "agent", "adopt", extWT, "--engine", "claude-code", "--task", "fix tests")
	if err == nil {
		t.Fatal("want an error adopting an unregistered worktree without --adopt-worktree")
	}
	if !strings.Contains(stderr, "--adopt-worktree") {
		t.Fatalf("stderr = %q, want the --adopt-worktree remedy named", stderr)
	}
}

// TestAgentAdoptWithAdoptWorktreeFlagHappyPath proves --adopt-worktree
// registers the worktree (via the SAME code path `worktree adopt` uses) and
// inserts a running agent_runs row against it, visible via `agent list`.
func TestAgentAdoptWithAdoptWorktreeFlagHappyPath(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	localPath := setupFreshWorktreeRepo(t, home, root, "auto", false)
	head := strings.TrimSpace(runGitOutput(t, localPath, "rev-parse", "HEAD"))
	extWT := filepath.Join(t.TempDir(), "external-wt")
	runGit(t, localPath, "worktree", "add", "--detach", extWT, head)

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "agent", "adopt", extWT, "--engine", "claude-code", "--task", "fix failing tests", "--adopt-worktree", "--json")
	if err != nil {
		t.Fatalf("agent adopt stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	var out agentAdoptResultForTest
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("decode agent adopt --json: %v\n%s", err, stdout)
	}
	if out.Engine != "claude-code" {
		t.Fatalf("Engine = %q, want claude-code", out.Engine)
	}
	if out.Task != "fix failing tests" {
		t.Fatalf("Task = %q, want %q", out.Task, "fix failing tests")
	}
	if out.Status != "running" {
		t.Fatalf("Status = %q, want running", out.Status)
	}
	if out.NamespaceID == "" || out.WorktreeID == "" {
		t.Fatalf("expected namespace/worktree ids to be populated: %+v", out)
	}
	if out.WorktreePath == "" {
		t.Fatalf("WorktreePath must be populated: %+v", out)
	}

	stdout, stderr, err = executeForTest("--home", home, "--root", root, "agent", "list", "--json")
	if err != nil {
		t.Fatalf("agent list stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	var runs []state.AgentRun
	if err := json.Unmarshal([]byte(stdout), &runs); err != nil {
		t.Fatalf("decode agent list --json: %v\n%s", err, stdout)
	}
	found := false
	for _, r := range runs {
		if r.ID == out.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("agent list = %+v, want to include adopted run %s", runs, out.ID)
	}
}

// TestAgentAdoptRecordsTargetPidStartTime proves --pid records the START
// TIME of the NAMED pid, never the current process's — the sweep/staleness
// check (processIdentityAlive) depends on this being the harness's own
// identity, not devstrap's.
func TestAgentAdoptRecordsTargetPidStartTime(t *testing.T) {
	const targetPID = 424242
	const targetStartedAt = int64(999999)
	oldProcessStartTime := processStartTime
	processStartTime = func(pid int) (int64, error) {
		if pid == targetPID {
			return targetStartedAt, nil
		}
		return 111111, nil // any other pid (e.g. a wrong os.Getpid() call) is distinguishable.
	}
	t.Cleanup(func() { processStartTime = oldProcessStartTime })

	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	localPath := setupFreshWorktreeRepo(t, home, root, "auto", false)
	head := strings.TrimSpace(runGitOutput(t, localPath, "rev-parse", "HEAD"))
	extWT := filepath.Join(t.TempDir(), "external-wt")
	runGit(t, localPath, "worktree", "add", "--detach", extWT, head)

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "agent", "adopt", extWT, "--engine", "claude-code", "--task", "fix tests", "--adopt-worktree", "--pid", itoa(targetPID), "--json")
	if err != nil {
		t.Fatalf("agent adopt stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	var out agentAdoptResultForTest
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("decode agent adopt --json: %v\n%s", err, stdout)
	}
	if out.RunnerPID != targetPID {
		t.Fatalf("RunnerPID = %d, want %d", out.RunnerPID, targetPID)
	}
	if out.RunnerStartedAt != targetStartedAt {
		t.Fatalf("RunnerStartedAt = %d, want %d (the TARGET pid's start time, not the current process's)", out.RunnerStartedAt, targetStartedAt)
	}
}

// TestAgentAdoptPidlessRecordsNoPid proves omitting --pid records neither a
// PID nor a start-time identity (spec/13: a pidless run is never swept by
// RunningAgentRunsWithPID and blocks worktree cleanup until `agent finish`).
func TestAgentAdoptPidlessRecordsNoPid(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	localPath := setupFreshWorktreeRepo(t, home, root, "auto", false)
	head := strings.TrimSpace(runGitOutput(t, localPath, "rev-parse", "HEAD"))
	extWT := filepath.Join(t.TempDir(), "external-wt")
	runGit(t, localPath, "worktree", "add", "--detach", extWT, head)

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "agent", "adopt", extWT, "--engine", "claude-code", "--task", "fix tests", "--adopt-worktree", "--json")
	if err != nil {
		t.Fatalf("agent adopt stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	var out agentAdoptResultForTest
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("decode agent adopt --json: %v\n%s", err, stdout)
	}
	if out.RunnerPID != 0 {
		t.Fatalf("RunnerPID = %d, want 0 (pidless)", out.RunnerPID)
	}
	if out.RunnerStartedAt != 0 {
		t.Fatalf("RunnerStartedAt = %d, want 0 (pidless)", out.RunnerStartedAt)
	}

	runs, err := state.Open(context.Background(), filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = runs.Close() }()
	pidRuns, err := runs.RunningAgentRunsWithPID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range pidRuns {
		if r.ID == out.ID {
			t.Fatalf("pidless run %s must not be selected by RunningAgentRunsWithPID", out.ID)
		}
	}
}

// insertAgentRunForTest inserts a bare agent_runs row directly against the
// store, bypassing the CLI, so `agent finish` transition tests can start from
// an arbitrary status without needing a real worktree.
func insertAgentRunForTest(t *testing.T, home, projectPath, status string) state.AgentRun {
	t.Helper()
	ctx := context.Background()
	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	project, err := store.ProjectByPath(ctx, projectPath)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.InsertAgentRun(ctx, state.AgentRun{
		NamespaceID: project.ID,
		Engine:      "claude-code",
		Task:        "finish transition fixture",
		Status:      status,
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func agentRunStatusForTest(t *testing.T, home, runID string) string {
	t.Helper()
	ctx := context.Background()
	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	run, err := store.AgentRunByID(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	return run.Status
}

// TestAgentFinishTransitions pins every documented status transition:
// running/interrupted move freely to complete/failed (interrupted->complete
// warns to stderr because the harness's own report outweighs the sweep's
// dead-PID inference), and finishing an already-terminal run refuses.
func TestAgentFinishTransitions(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	_ = setupFreshWorktreeRepo(t, home, root, "auto", false)

	cases := []struct {
		name       string
		from       string
		flagStatus string
		wantErr    bool
		wantWarn   bool
	}{
		{name: "running to complete", from: "running", flagStatus: "complete"},
		{name: "running to failed", from: "running", flagStatus: "failed"},
		{name: "interrupted to failed", from: "interrupted", flagStatus: "failed"},
		{name: "interrupted to complete warns", from: "interrupted", flagStatus: "complete", wantWarn: true},
		{name: "already complete refuses", from: "complete", flagStatus: "complete", wantErr: true},
		{name: "already failed refuses", from: "failed", flagStatus: "failed", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := insertAgentRunForTest(t, home, "work/acme/repo", tc.from)
			stdout, stderr, err := executeForTest("--home", home, "--root", root, "agent", "finish", run.ID, "--status", tc.flagStatus, "--json")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want a refusal finishing a %s run, stdout=%q", tc.from, stdout)
				}
				if !strings.Contains(stderr, "already") {
					t.Fatalf("stderr = %q, want an 'already <status>' refusal", stderr)
				}
				if got := agentRunStatusForTest(t, home, run.ID); got != tc.from {
					t.Fatalf("status = %q, want unchanged %q after a refused finish", got, tc.from)
				}
				return
			}
			if err != nil {
				t.Fatalf("agent finish stdout=%q stderr=%q err=%v", stdout, stderr, err)
			}
			hasWarn := strings.Contains(stderr, "warning:")
			if hasWarn != tc.wantWarn {
				t.Fatalf("stderr = %q, wantWarn = %v", stderr, tc.wantWarn)
			}
			var out agentFinishResultForTest
			if err := json.Unmarshal([]byte(stdout), &out); err != nil {
				t.Fatalf("decode agent finish --json: %v\n%s", err, stdout)
			}
			if out.Status != tc.flagStatus {
				t.Fatalf("--json Status = %q, want %q", out.Status, tc.flagStatus)
			}
			if out.SchemaVersion != agentFinishSchemaVersion {
				t.Fatalf("SchemaVersion = %d, want %d", out.SchemaVersion, agentFinishSchemaVersion)
			}
			if got := agentRunStatusForTest(t, home, run.ID); got != tc.flagStatus {
				t.Fatalf("status = %q, want %q", got, tc.flagStatus)
			}
		})
	}
}

func TestAgentFinishRejectsUnsupportedStatus(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	_ = setupFreshWorktreeRepo(t, home, root, "auto", false)
	run := insertAgentRunForTest(t, home, "work/acme/repo", "running")

	_, stderr, err := executeForTest("--home", home, "--root", root, "agent", "finish", run.ID, "--status", "bogus")
	if err == nil {
		t.Fatal("want an error for an unsupported --status")
	}
	if !strings.Contains(stderr, "unsupported --status") {
		t.Fatalf("stderr = %q, want an unsupported-status refusal", stderr)
	}
}

// TestAgentPRReachableAfterAgentFinishComplete proves the whole point of
// `agent finish`: after it records status=complete, `agent pr` works with NO
// --allow-incomplete, exactly as it would for a run `agent run` completed
// itself.
func TestAgentPRReachableAfterAgentFinishComplete(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	localPath := setupFreshWorktreeRepo(t, home, root, "auto", false)

	// A branch (not detached) worktree, so `agent pr` does not refuse on the
	// unrelated branchless gate (TestAgentPrRefusesBranchlessAdoptedWorktree).
	extWT := filepath.Join(t.TempDir(), "external-wt")
	runGit(t, localPath, "worktree", "add", "-b", "agent/adopted-feature", extWT, "HEAD")

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "agent", "adopt", extWT, "--engine", "claude-code", "--task", "fix failing tests", "--adopt-worktree", "--json")
	if err != nil {
		t.Fatalf("agent adopt stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	var adopted agentAdoptResultForTest
	if err := json.Unmarshal([]byte(stdout), &adopted); err != nil {
		t.Fatalf("decode agent adopt --json: %v\n%s", err, stdout)
	}

	if _, stderr, err := executeForTest("--home", home, "--root", root, "agent", "finish", adopted.ID, "--status", "complete", "--test-summary", "42 passed"); err != nil {
		t.Fatalf("agent finish stderr=%q err=%v", stderr, err)
	}

	stdout, stderr, err = executeForTest("--home", home, "--root", root, "agent", "pr", adopted.ID, "--dry-run")
	if err != nil {
		t.Fatalf("agent pr stdout=%q stderr=%q err=%v (must succeed with no --allow-incomplete after agent finish --status complete)", stdout, stderr, err)
	}
	if !strings.Contains(stdout, "Would create PR") {
		t.Fatalf("stdout = %q, want the dry-run PR preview", stdout)
	}
}
