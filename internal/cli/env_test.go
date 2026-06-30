package cli

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
)

// P5-PROD-03: `env rotate --all` clears the needs-rotation flag set by a device
// revoke, so `doctor` can return to green after the operator rotates at source.
func TestEnvRotateAllClearsRotationFlag(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--workspace-name", "personal"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	ctx := context.Background()
	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertProject(ctx, state.UpsertProjectParams{Path: "work/x", Type: "git_repo", RemoteKey: "github.com/acme/x", RemoteURL: "https://github.com/acme/x"}); err != nil {
		t.Fatal(err)
	}
	proj, err := store.ProjectByPath(ctx, "work/x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveCapturedEnvProfile(ctx, proj.ID, "default", []string{"API_KEY"}, "age_blob:"+hex64a); err != nil {
		t.Fatal(err)
	}
	flagged, err := store.MarkEncryptedBindingsNeedingRotation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if flagged != 1 {
		t.Fatalf("flagged = %d, want 1", flagged)
	}
	closeStore(store)

	if _, stderr, err := executeForTest("--home", home, "--root", root, "env", "rotate", "--all"); err != nil {
		t.Fatalf("env rotate --all stderr = %q err = %v", stderr, err)
	}

	store2, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store2)
	if err := store2.Migrate(); err != nil {
		t.Fatal(err)
	}
	remaining, err := store2.CountSecretBindingsNeedingRotation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("bindings needing rotation after env rotate --all = %d, want 0", remaining)
	}
}
