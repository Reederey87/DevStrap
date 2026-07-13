package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
)

// P5-SYNC-04: `conflicts resolve --keep-remote` is authoritative — it switches
// the project at the path to the alternate (non-winning) remote variant and
// closes the conflict, instead of only recording the choice.
func TestConflictResolveKeepRemoteSwitchesVariant(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--workspace-name", "personal"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	store, err := state.Open(context.Background(), filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Two competing remote events at the same path, different remotes, create a
	// same_path_different_remote conflict. The deterministic winner is the higher
	// coordinate (device-y @20).
	const hlcShift = 16
	ev1, err := dssync.NewProjectEvent("device-x", dssync.EventProjectAdded, (realisticTestPhysicalMS+10)<<hlcShift, dssync.ProjectPayload{
		Path: "work/acme/api", Type: "git_repo", RemoteKey: "github.com/acme/api", RemoteURL: "https://github.com/acme/api",
	})
	if err != nil {
		t.Fatal(err)
	}
	ev2, err := dssync.NewProjectEvent("device-y", dssync.EventProjectAdded, (realisticTestPhysicalMS+20)<<hlcShift, dssync.ProjectPayload{
		Path: "work/acme/api", Type: "git_repo", RemoteKey: "gitlab.com/acme/api", RemoteURL: "https://gitlab.com/acme/api",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dssync.ApplyEvents(ctx, store, []state.Event{ev1, ev2}); err != nil {
		t.Fatalf("apply competing events: %v", err)
	}
	open, err := store.OpenConflicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("open conflicts = %d, want 1", len(open))
	}
	conflictID := open[0].ID
	winner, err := store.ProjectByPath(ctx, "work/acme/api")
	if err != nil {
		t.Fatal(err)
	}
	closeStore(store)

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "conflicts", "resolve", conflictID, "--keep-remote")
	if err != nil {
		t.Fatalf("resolve stderr = %q err = %v", stderr, err)
	}
	if !strings.Contains(stdout, "resolved (keep-remote)") {
		t.Fatalf("resolve stdout = %q, want resolved (keep-remote)", stdout)
	}

	store2, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store2)
	if err := store2.Migrate(); err != nil {
		t.Fatal(err)
	}
	got, err := store2.ProjectByPath(ctx, "work/acme/api")
	if err != nil {
		t.Fatal(err)
	}
	if got.RemoteKey == winner.RemoteKey {
		t.Fatalf("keep-remote did not switch the variant (still %q)", winner.RemoteKey)
	}
	if got.RemoteKey != "github.com/acme/api" {
		t.Fatalf("project remote = %q, want github.com/acme/api", got.RemoteKey)
	}
	if remaining, _ := store2.OpenConflicts(ctx); len(remaining) != 0 {
		t.Fatalf("conflict still open after resolve = %d, want 0", len(remaining))
	}
}

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
	_, _, err = executeForTest("--home", home, "conflicts", "resolve", conflictID)
	if err == nil {
		t.Fatal("expected usage error when no keep-* flag is set")
	}
}
