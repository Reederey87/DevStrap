package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHubMigrateEventsJSON pins the P5-CLI-01 part B --json shape for
// hub migrate-events (file-backed hubs never used the legacy layout → 0/0).
func TestHubMigrateEventsJSON(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	hubPath := filepath.Join(t.TempDir(), "hub.json")
	if err := os.WriteFile(hubPath, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init: %v (%s)", err, stderr)
	}

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json", "hub", "migrate-events", "--hub-file", hubPath, "--dry-run")
	if err != nil {
		t.Fatalf("hub migrate-events --json: %v (%s)", err, stderr)
	}
	var got hubMigrateEventsResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("hub migrate-events --json is not a hubMigrateEventsResult: %v\n%s", err, stdout)
	}
	if !got.DryRun {
		t.Error("dry_run = false, want true")
	}
	if got.Migrated != 0 || got.Kept != 0 {
		t.Errorf("file hub migrate-events = (%d, %d), want (0, 0)", got.Migrated, got.Kept)
	}
	if strings.Contains(stdout, "hub migrate-events:") {
		t.Fatalf("hub migrate-events --json leaked human summary: %s", stdout)
	}
}

// TestHubMigrateEventsHumanMode still prints the pre-migration summary line
// (human output unchanged by the --json wiring).
func TestHubMigrateEventsHumanMode(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	hubPath := filepath.Join(t.TempDir(), "hub.json")
	if err := os.WriteFile(hubPath, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init: %v (%s)", err, stderr)
	}

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "hub", "migrate-events", "--hub-file", hubPath, "--dry-run")
	if err != nil {
		t.Fatalf("hub migrate-events: %v (%s)", err, stderr)
	}
	want := "hub migrate-events: would migrate 0 legacy event(s); kept 0 unmigratable object(s)\n"
	if stdout != want {
		t.Fatalf("human stdout = %q, want %q", stdout, want)
	}
}
