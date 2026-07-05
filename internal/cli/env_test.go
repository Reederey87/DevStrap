package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
)

func TestCaptureEnvProfileEmitsEnvEvent(t *testing.T) {
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
	closeStore(store)
	projectDir := filepath.Join(root, "work", "x")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".env"), []byte("API_KEY=marker\nDB_URL=postgres://local\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, stderr, err := executeForTest("--home", home, "--root", root, "env", "capture", "work/x", ".env"); err != nil {
		t.Fatalf("env capture stderr = %q err = %v", stderr, err)
	}

	store2, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store2)
	if err := store2.Migrate(); err != nil {
		t.Fatal(err)
	}
	events, err := store2.PendingEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var envEvents []state.Event
	for _, event := range events {
		if event.Type == dssync.EventEnvProfileUpdated {
			envEvents = append(envEvents, event)
		}
	}
	if len(envEvents) != 1 {
		t.Fatalf("env event count = %d, want 1; events=%#v", len(envEvents), events)
	}
	var payload dssync.EnvProfilePayload
	if err := json.Unmarshal([]byte(envEvents[0].PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Path != "work/x" || payload.Profile != "default" || payload.Provider != "devstrap_encrypted" || payload.BlobRef == "" {
		t.Fatalf("payload = %#v", payload)
	}
	if strings.Join(payload.VarNames, ",") != "API_KEY,DB_URL" {
		t.Fatalf("payload vars = %v", payload.VarNames)
	}
	project, err := store2.ProjectByPath(ctx, "work/x")
	if err != nil {
		t.Fatal(err)
	}
	profile, bindings, err := store2.EnvProfileForProject(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Provider != "devstrap_encrypted" || len(bindings) != 2 {
		t.Fatalf("profile=%#v bindings=%#v", profile, bindings)
	}
}

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

func TestEnvRotateProjectClearsRotationFlag(t *testing.T) {
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
	if _, err := store.SaveCapturedEnvProfile(ctx, proj.ID, "default", []string{"API_KEY", "DB_URL"}, "age_blob:"+hex64a); err != nil {
		t.Fatal(err)
	}
	flagged, err := store.MarkEncryptedBindingsNeedingRotation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if flagged != 2 {
		t.Fatalf("flagged = %d, want 2", flagged)
	}
	closeStore(store)

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "env", "rotate", "work/x")
	if err != nil {
		t.Fatalf("env rotate stderr = %q err = %v", stderr, err)
	}
	if want := "Cleared the needs-rotation flag on 2 binding(s) for work/x"; !strings.Contains(stdout, want) {
		t.Fatalf("stdout = %q, want %q", stdout, want)
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
		t.Fatalf("bindings needing rotation after env rotate = %d, want 0", remaining)
	}
}
