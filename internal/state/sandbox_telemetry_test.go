package state

import (
	"context"
	"path/filepath"
	"testing"
)

func openSandboxTelemetryTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(context.Background(), "personal", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureDevice(context.Background(), "test-device"); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestAgentRunSandboxColumnsRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := openSandboxTelemetryTestStore(t)
	ns, err := st.UpsertProject(ctx, UpsertProjectParams{Path: "work/sandbox-columns", Type: "plain_folder"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.InsertAgentRun(ctx, AgentRun{
		ID:                 "arun_sandbox_columns",
		NamespaceID:        ns.ID,
		Engine:             "generic",
		Task:               "sandbox columns",
		Status:             "running",
		SandboxBackend:     "seatbelt",
		SandboxMode:        "require",
		SandboxLimitations: `["lim-a","lim-b"]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.AgentRunByID(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SandboxBackend != "seatbelt" || got.SandboxMode != "require" || got.SandboxLimitations != `["lim-a","lim-b"]` {
		t.Fatalf("sandbox columns = backend %q mode %q limitations %q", got.SandboxBackend, got.SandboxMode, got.SandboxLimitations)
	}
}

func TestSandboxViolationsRoundTripAndCount(t *testing.T) {
	ctx := context.Background()
	st := openSandboxTelemetryTestStore(t)
	ns, err := st.UpsertProject(ctx, UpsertProjectParams{Path: "work/sandbox-violations", Type: "plain_folder"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.InsertAgentRun(ctx, AgentRun{
		ID:          "arun_sandbox_violations",
		NamespaceID: ns.ID,
		Engine:      "generic",
		Task:        "sandbox violations",
		Status:      "running",
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := []SandboxViolation{
		{RunID: run.ID, ObservedAt: "2026-07-05T12:00:00.000000000Z", Backend: "seatbelt", Operation: "file-write-create", Path: "/tmp/outside", Detail: "raw one", Source: "seatbelt-log"},
		{RunID: run.ID, ObservedAt: "2026-07-05T12:00:01.000000000Z", Backend: "seatbelt", Operation: "file-read-data", Source: "seatbelt-log"},
	}
	if err := st.InsertSandboxViolations(ctx, rows); err != nil {
		t.Fatal(err)
	}
	got, err := st.SandboxViolationsByRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("violations = %+v, want 2", got)
	}
	if got[0].Operation != "file-write-create" || got[0].Path != "/tmp/outside" || got[0].Detail != "raw one" {
		t.Fatalf("first violation = %+v", got[0])
	}
	if got[1].Operation != "file-read-data" || got[1].Path != "" || got[1].Detail != "" {
		t.Fatalf("second violation = %+v", got[1])
	}
	count, err := st.CountRunsWithSandboxViolations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("CountRunsWithSandboxViolations = %d, want 1", count)
	}
}
