package state

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRunningAgentRunsByWorktree(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(ctx, "personal", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	device, err := st.EnsureDevice(ctx, "test-device")
	if err != nil {
		t.Fatal(err)
	}
	ns, err := st.UpsertProject(ctx, UpsertProjectParams{Path: "work/agent-wt-query", Type: "plain_folder"})
	if err != nil {
		t.Fatal(err)
	}
	wtA, err := st.InsertWorktree(ctx, Worktree{
		ID:          "wt_running_a",
		NamespaceID: ns.ID,
		DeviceID:    device.ID,
		Path:        "/tmp/wt-a",
		Branch:      "agent/a",
		BaseRef:     "origin/main",
		BaseSHA:     "abc",
		CreatedBy:   "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	wtB, err := st.InsertWorktree(ctx, Worktree{
		ID:          "wt_running_b",
		NamespaceID: ns.ID,
		DeviceID:    device.ID,
		Path:        "/tmp/wt-b",
		Branch:      "agent/b",
		BaseRef:     "origin/main",
		BaseSHA:     "abc",
		CreatedBy:   "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Running on A (no PID still counts — cleanup must block).
	if _, err := st.InsertAgentRun(ctx, AgentRun{
		ID:          "arun_on_a",
		NamespaceID: ns.ID,
		WorktreeID:  wtA.ID,
		Engine:      "generic",
		Task:        "live",
		Status:      "running",
	}); err != nil {
		t.Fatal(err)
	}
	// Succeeded on A must not match.
	if _, err := st.InsertAgentRun(ctx, AgentRun{
		ID:          "arun_done_a",
		NamespaceID: ns.ID,
		WorktreeID:  wtA.ID,
		Engine:      "generic",
		Task:        "done",
		Status:      "succeeded",
	}); err != nil {
		t.Fatal(err)
	}
	// Running on B must not match A.
	if _, err := st.InsertAgentRun(ctx, AgentRun{
		ID:          "arun_on_b",
		NamespaceID: ns.ID,
		WorktreeID:  wtB.ID,
		Engine:      "generic",
		Task:        "other",
		Status:      "running",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := st.RunningAgentRunsByWorktree(ctx, wtA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "arun_on_a" {
		t.Fatalf("RunningAgentRunsByWorktree(%s) = %+v, want only arun_on_a", wtA.ID, got)
	}
	gotB, err := st.RunningAgentRunsByWorktree(ctx, wtB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotB) != 1 || gotB[0].ID != "arun_on_b" {
		t.Fatalf("RunningAgentRunsByWorktree(%s) = %+v, want only arun_on_b", wtB.ID, gotB)
	}
	empty, err := st.RunningAgentRunsByWorktree(ctx, "wt_missing")
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("missing worktree runs = %+v, want empty", empty)
	}
}
