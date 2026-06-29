package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
)

func TestResolveActionValidation(t *testing.T) {
	if _, err := resolveAction(false, false, false); err == nil {
		t.Fatal("expected error when no keep-* flag is set")
	}
	if _, err := resolveAction(true, true, false); err == nil {
		t.Fatal("expected error when multiple keep-* flags are set")
	}
	if action, err := resolveAction(false, false, true); err != nil || action != "keep-both" {
		t.Fatalf("resolveAction(keep-both) = (%q,%v), want (keep-both,nil)", action, err)
	}
}

func TestConflictsListShowResolve(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--workspace-name", "personal"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	// Seed an open conflict directly through the store.
	store, err := state.Open(context.Background(), filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
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
	closeStore(store)

	// list shows the seeded conflict.
	stdout, stderr, err := executeForTest("--home", home, "conflicts", "list")
	if err != nil {
		t.Fatalf("conflicts list stderr = %q err = %v", stderr, err)
	}
	if !strings.Contains(stdout, conflictID) || !strings.Contains(stdout, "same-path/different-remote") {
		t.Fatalf("conflicts list stdout = %q, want conflict %s", stdout, conflictID)
	}

	// `devstrap conflicts` (no subcommand) also lists.
	stdout, _, err = executeForTest("--home", home, "conflicts")
	if err != nil {
		t.Fatalf("conflicts (default) err = %v", err)
	}
	if !strings.Contains(stdout, conflictID) {
		t.Fatalf("conflicts default stdout = %q, want conflict %s", stdout, conflictID)
	}

	// show prints details + status.
	stdout, stderr, err = executeForTest("--home", home, "conflicts", "show", conflictID)
	if err != nil {
		t.Fatalf("conflicts show stderr = %q err = %v", stderr, err)
	}
	if !strings.Contains(stdout, "Status: open") || !strings.Contains(stdout, conflictID) {
		t.Fatalf("conflicts show stdout = %q, want status open + id", stdout)
	}

	// resolve --keep-local clears the row and emits the audit event.
	stdout, stderr, err = executeForTest("--home", home, "conflicts", "resolve", conflictID, "--keep-local")
	if err != nil {
		t.Fatalf("conflicts resolve stderr = %q err = %v", stderr, err)
	}
	if !strings.Contains(stdout, "resolved (keep-local)") {
		t.Fatalf("conflicts resolve stdout = %q, want resolved (keep-local)", stdout)
	}

	// list now reports no open conflicts.
	stdout, _, err = executeForTest("--home", home, "conflicts", "list")
	if err != nil {
		t.Fatalf("conflicts list after resolve err = %v", err)
	}
	if !strings.Contains(stdout, "No open conflicts.") {
		t.Fatalf("conflicts list after resolve stdout = %q, want no open conflicts", stdout)
	}

	// resolving an already-resolved conflict errors.
	_, stderr, err = executeForTest("--home", home, "conflicts", "resolve", conflictID, "--keep-remote")
	if err == nil {
		t.Fatal("expected error resolving an already-resolved conflict")
	}
	if !strings.Contains(stderr, "already resolved") {
		t.Fatalf("stderr = %q, want already resolved", stderr)
	}

	// resolve with no keep-* flag is a usage error.
	_, stderr, err = executeForTest("--home", home, "conflicts", "resolve", conflictID)
	if err == nil {
		t.Fatal("expected usage error when no keep-* flag is set")
	}
}
