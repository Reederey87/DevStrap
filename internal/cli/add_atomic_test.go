package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/viper"
)

func TestAddProjectEventAndEntryAtomic(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := filepath.Join(t.TempDir(), ".devstrap")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureWorkspace(ctx, "test", root); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureDevice(ctx, "device-a"); err != nil {
		t.Fatal(err)
	}
	v := viper.New()
	v.Set("home", home)
	v.Set("root", root)
	opts := &options{v: v}

	project, err := addProject(ctx, store, opts, "git@github.com:acme/api.git", "work/acme/api", "main", "auto")
	if err != nil {
		t.Fatal(err)
	}
	if project.SourceEventID == "" {
		t.Fatalf("project source event id is empty: %+v", project)
	}
	event, err := store.EventByID(ctx, project.SourceEventID)
	if err != nil {
		t.Fatalf("source event was not committed: %v", err)
	}
	if event.HLC != project.SourceEventHLC || event.DeviceID != project.SourceEventDeviceID {
		t.Fatalf("event/source mismatch: event=%+v project=%+v", event, project)
	}
	projects, err := store.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].SourceEventID != event.ID {
		t.Fatalf("projects = %+v, want one row sourced by %s", projects, event.ID)
	}
}
