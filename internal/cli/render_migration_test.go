package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Reederey87/DevStrap/internal/scan"
	"github.com/Reederey87/DevStrap/internal/state"
)

// These tests pin the P5-CLI-01 migration rule: each command's --json output
// shape must stay byte-for-byte identical after moving from an inline
// json.NewEncoder call to the shared opts.render seam. `worktree list` and
// `service status` already have --json coverage elsewhere (worktree_test.go's
// listWorktreesForTest, service_test.go's TestServiceStatusJSON) and are not
// duplicated here.

func TestAgentListJSON(t *testing.T) {
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
	ns, err := store.UpsertProject(ctx, state.UpsertProjectParams{Path: "work/acme/api", Type: "plain_folder"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertAgentRun(ctx, state.AgentRun{ID: "arun_list_json", NamespaceID: ns.ID, Engine: "generic", Task: "run tests", Status: "complete"}); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	stdout, stderr, err := executeForTest("--home", home, "--json", "agent", "list")
	if err != nil {
		t.Fatalf("agent list stderr = %q err = %v", stderr, err)
	}
	var runs []state.AgentRun
	if err := json.Unmarshal([]byte(stdout), &runs); err != nil {
		t.Fatalf("agent list --json is not a bare array of state.AgentRun: %v\n%s", err, stdout)
	}
	if len(runs) != 1 || runs[0].ID != "arun_list_json" || runs[0].Task != "run tests" {
		t.Fatalf("agent list --json = %+v, want one run arun_list_json", runs)
	}
}

func TestAgentShowJSON(t *testing.T) {
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
	ns, err := store.UpsertProject(ctx, state.UpsertProjectParams{Path: "work/acme/api", Type: "plain_folder"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.InsertAgentRun(ctx, state.AgentRun{ID: "arun_show_json", NamespaceID: ns.ID, Engine: "generic", Task: "run tests", Status: "complete"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertSandboxViolations(ctx, []state.SandboxViolation{{
		RunID:      run.ID,
		ObservedAt: state.TimestampNow(),
		Backend:    "seatbelt",
		Operation:  "file-write-create",
		Source:     "seatbelt-log",
	}}); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	stdout, stderr, err := executeForTest("--home", home, "--json", "agent", "show", "arun_show_json")
	if err != nil {
		t.Fatalf("agent show stderr = %q err = %v", stderr, err)
	}
	var got struct {
		state.AgentRun
		Violations []state.SandboxViolation `json:"violations"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("agent show --json is not the expected embedded shape: %v\n%s", err, stdout)
	}
	if got.ID != "arun_show_json" || len(got.Violations) != 1 || got.Violations[0].Operation != "file-write-create" {
		t.Fatalf("agent show --json = %+v, want run arun_show_json with 1 violation", got)
	}
}

func TestConflictsListJSON(t *testing.T) {
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
	_ = store.Close()

	stdout, stderr, err := executeForTest("--home", home, "--json", "conflicts", "list")
	if err != nil {
		t.Fatalf("conflicts list stderr = %q err = %v", stderr, err)
	}
	var conflicts []state.Conflict
	if err := json.Unmarshal([]byte(stdout), &conflicts); err != nil {
		t.Fatalf("conflicts list --json is not a bare array of state.Conflict: %v\n%s", err, stdout)
	}
	if len(conflicts) != 1 || conflicts[0].Type != "same-path/different-remote" {
		t.Fatalf("conflicts list --json = %+v, want one same-path/different-remote conflict", conflicts)
	}
}

func TestConflictsShowJSON(t *testing.T) {
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

	stdout, stderr, err := executeForTest("--home", home, "--json", "conflicts", "show", conflictID)
	if err != nil {
		t.Fatalf("conflicts show stderr = %q err = %v", stderr, err)
	}
	var got state.Conflict
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("conflicts show --json is not a bare state.Conflict object: %v\n%s", err, stdout)
	}
	if got.ID != conflictID || got.Type != "same-path/different-remote" {
		t.Fatalf("conflicts show --json = %+v, want conflict %s", got, conflictID)
	}
}

func TestDevicesListJSON(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}

	stdout, stderr, err := executeForTest("--home", home, "--json", "devices", "list")
	if err != nil {
		t.Fatalf("devices list stderr = %q err = %v", stderr, err)
	}
	var devices []state.Device
	if err := json.Unmarshal([]byte(stdout), &devices); err != nil {
		t.Fatalf("devices list --json is not a bare array of state.Device: %v\n%s", err, stdout)
	}
	if len(devices) != 1 || devices[0].ID == "" {
		t.Fatalf("devices list --json = %+v, want the one local device", devices)
	}
}

func TestDoctorJSON(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json", "doctor")
	if err != nil && stderr == "" {
		// doctor exits non-zero on warnings/errors; only fail on a genuinely
		// broken invocation (empty stderr AND an error).
		t.Fatalf("doctor err = %v with no stderr", err)
	}
	var results []checkResult
	if jsonErr := json.Unmarshal([]byte(stdout), &results); jsonErr != nil {
		t.Fatalf("doctor --json is not a bare array of checkResult: %v\n%s", jsonErr, stdout)
	}
	if len(results) == 0 {
		t.Fatalf("doctor --json returned no checks")
	}
	for _, r := range results {
		if r.Name == "" || r.Status == "" {
			t.Fatalf("doctor --json check missing name/status: %+v", r)
		}
	}
}

func TestScanJSON(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}

	stdout, stderr, err := executeForTest("--home", home, "--json", "scan", root)
	if err != nil {
		t.Fatalf("scan stderr = %q err = %v", stderr, err)
	}
	var result scan.Result
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("scan --json is not a bare scan.Result object: %v\n%s", err, stdout)
	}
	if result.Root != root {
		t.Fatalf("scan --json Root = %q, want %q", result.Root, root)
	}
}

func TestWorktreeUnlockJSON(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "add", "git@github.com:acme/api.git", "--path", "work/acme/api"); err != nil {
		t.Fatalf("add stderr = %q err = %v", stderr, err)
	}

	// No lock file exists for this project: report.Held must be false.
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json", "worktree", "unlock", "work/acme/api")
	if err != nil {
		t.Fatalf("worktree unlock stderr = %q err = %v", stderr, err)
	}
	var report repoLockReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("worktree unlock --json is not a bare repoLockReport object: %v\n%s", err, stdout)
	}
	if report.Held || report.Cleared {
		t.Fatalf("worktree unlock --json = %+v, want held=false cleared=false (no lock existed)", report)
	}
}

func TestWorktreeStatusJSON(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	setupFreshWorktreeRepo(t, home, root, "auto", false)

	if _, stderr, err := executeForTest("--home", home, "--root", root, "worktree", "new", "work/acme/repo", "--fresh-upstream", "--name", "status json"); err != nil {
		t.Fatalf("worktree new stderr = %q err = %v", stderr, err)
	}
	worktrees := listWorktreesForTest(t, home, root)
	if len(worktrees) != 1 {
		t.Fatalf("worktree count = %d, want 1: %+v", len(worktrees), worktrees)
	}
	wtID := worktrees[0].ID

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json", "worktree", "status", wtID)
	if err != nil {
		t.Fatalf("worktree status stderr = %q err = %v", stderr, err)
	}
	var out worktreeStatusOutput
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("worktree status --json is not a bare worktreeStatusOutput object: %v\n%s", err, stdout)
	}
	if out.ID != wtID {
		t.Fatalf("worktree status --json ID = %q, want %q", out.ID, wtID)
	}
}

func TestStatusJSON(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--workspace-name", "personal"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json", "status")
	if err != nil {
		t.Fatalf("status stderr = %q err = %v", stderr, err)
	}
	var summary state.Summary
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil {
		t.Fatalf("status --json is not a bare state.Summary object: %v\n%s", err, stdout)
	}
	if summary.WorkspaceName != "personal" || summary.RootPath != root {
		t.Fatalf("status --json = %+v, want workspace_name=personal root_path=%s", summary, root)
	}
}
