package cli

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
)

func TestCheckSandboxViolations(t *testing.T) {
	ctx := context.Background()
	st, err := state.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
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
	if _, err := st.EnsureDevice(ctx, "test-device"); err != nil {
		t.Fatal(err)
	}

	ok := checkSandboxViolations(ctx, st)
	if len(ok) != 1 || ok[0].Status != checkOK || ok[0].Detail != "0" {
		t.Fatalf("empty check = %+v, want OK/0", ok)
	}

	ns, err := st.UpsertProject(ctx, state.UpsertProjectParams{Path: "work/doctor-sandbox", Type: "plain_folder"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.InsertAgentRun(ctx, state.AgentRun{ID: "arun_doctor_sandbox", NamespaceID: ns.ID, Engine: "generic", Task: "doctor", Status: "complete"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertSandboxViolations(ctx, []state.SandboxViolation{{
		RunID:      run.ID,
		ObservedAt: state.TimestampNow(),
		Backend:    "seatbelt",
		Operation:  "file-write-create",
		Source:     "seatbelt-log",
	}}); err != nil {
		t.Fatal(err)
	}
	warn := checkSandboxViolations(ctx, st)
	if len(warn) != 1 || warn[0].Status != checkWarn || warn[0].Detail != "1 run(s) with denials" || warn[0].Remedy == "" {
		t.Fatalf("warn check = %+v", warn)
	}
}
