package state

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// P4-SEC-07 pairing: a joiner adopts the founder's workspace id at init via
// EnsureWorkspaceWithID. The id must be born-correct — a store initialized
// under a different id is refused, never rewritten.

const testWorkspaceID = "ws_0123456789abcdef0123456789abcdef"

func openWorkspaceIDTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestEnsureWorkspaceWithIDAdoptsSuppliedID(t *testing.T) {
	ctx := context.Background()
	st := openWorkspaceIDTestStore(t)
	if err := st.EnsureWorkspaceWithID(ctx, testWorkspaceID, "personal", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	got, err := st.WorkspaceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != testWorkspaceID {
		t.Fatalf("WorkspaceID = %q, want %q", got, testWorkspaceID)
	}
	// The in-process memo must be set exactly as EnsureWorkspace sets it.
	st.workspaceMu.RLock()
	memo := st.workspaceID
	st.workspaceMu.RUnlock()
	if memo != testWorkspaceID {
		t.Fatalf("workspaceID memo = %q, want %q", memo, testWorkspaceID)
	}
	// Child rows keyed by workspace_id must attach cleanly (FK safety).
	if _, err := st.EnsureDevice(ctx, "test-device"); err != nil {
		t.Fatal(err)
	}
	project, err := st.UpsertProject(ctx, UpsertProjectParams{
		Path:      "work/acme/repo",
		Type:      "git_repo",
		RemoteURL: "git@github.com:acme/repo.git",
		RemoteKey: "github.com/acme/repo",
		LocalPath: filepath.Join(t.TempDir(), "repo"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if project.ID == "" {
		t.Fatal("project row missing after adopt")
	}
	if check, err := st.ForeignKeyCheck(ctx); err != nil || check != "ok" {
		t.Fatalf("foreign key check = %q, %v", check, err)
	}
}

func TestEnsureWorkspaceWithIDIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st := openWorkspaceIDTestStore(t)
	if err := st.EnsureWorkspaceWithID(ctx, testWorkspaceID, "personal", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspaceWithID(ctx, testWorkspaceID, "renamed", "/tmp/Code2"); err != nil {
		t.Fatalf("idempotent re-ensure: %v", err)
	}
	if _, err := st.EnsureDevice(ctx, "test-device"); err != nil {
		t.Fatal(err)
	}
	summary, err := st.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.WorkspaceID != testWorkspaceID {
		t.Fatalf("Summary.WorkspaceID = %q, want %q", summary.WorkspaceID, testWorkspaceID)
	}
	if summary.WorkspaceName != "renamed" || summary.RootPath != "/tmp/Code2" {
		t.Fatalf("summary = %+v, want renamed name and root", summary)
	}
}

func TestEnsureWorkspaceWithIDRefusesDifferentID(t *testing.T) {
	ctx := context.Background()
	st := openWorkspaceIDTestStore(t)
	if err := st.EnsureWorkspace(ctx, "personal", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	minted, err := st.WorkspaceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = st.EnsureWorkspaceWithID(ctx, testWorkspaceID, "personal", "/tmp/Code")
	if !errors.Is(err, ErrWorkspaceIDMismatch) {
		t.Fatalf("err = %v, want ErrWorkspaceIDMismatch", err)
	}
	// The refusal must not clobber the memo or the stored id.
	got, err := st.WorkspaceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != minted {
		t.Fatalf("WorkspaceID after refusal = %q, want %q", got, minted)
	}
}

func TestEnsureWorkspaceDelegatesToAdoptedID(t *testing.T) {
	ctx := context.Background()
	st := openWorkspaceIDTestStore(t)
	if err := st.EnsureWorkspaceWithID(ctx, testWorkspaceID, "personal", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	// The mint path must keep the adopted id on re-init without a supplied id.
	if err := st.EnsureWorkspace(ctx, "personal", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	got, err := st.WorkspaceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != testWorkspaceID {
		t.Fatalf("WorkspaceID = %q, want adopted %q", got, testWorkspaceID)
	}
}

func TestEnsureWorkspaceWithIDRejectsEmptyID(t *testing.T) {
	st := openWorkspaceIDTestStore(t)
	if err := st.EnsureWorkspaceWithID(context.Background(), "", "personal", "/tmp/Code"); err == nil {
		t.Fatal("empty workspace id accepted, want error")
	}
}
