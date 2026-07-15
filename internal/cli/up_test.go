package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/state"
)

// mustGit runs a git command in dir (empty dir = no -C) and fails the test on error.
func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=devstrap-test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=devstrap-test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_TERMINAL_PROMPT=0",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// TestUpBootstrapsAdoptsAndSyncs covers the founder-side one-shot bootstrap end
// to end: `up --hub file:<hub>` founds the workspace, configures the hub in
// config.yaml, adopts a pre-populated repo via the default --scan, and completes
// a sync (founding epoch 1 on the empty hub).
func TestUpBootstrapsAdoptsAndSyncs(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	ctx := context.Background()

	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	hubPath := filepath.Join(t.TempDir(), "hub.json")
	hubURI := "file:" + hubPath

	// A real git project inside the root so `up --scan` adopts it.
	remote := filepath.Join(t.TempDir(), "remote.git")
	mustGit(t, "", "init", "--bare", "-b", "main", remote)
	proj := filepath.Join(root, "work", "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, proj, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(proj, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, proj, "add", "README.md")
	mustGit(t, proj, "commit", "-m", "init")
	mustGit(t, proj, "remote", "add", "origin", remote)
	mustGit(t, proj, "push", "origin", "main")

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "up", "--hub", hubURI)
	if err != nil {
		t.Fatalf("up: %v\nstderr=%s", err, stderr)
	}

	// Hub configured in config.yaml (persisted for later standalone runs).
	cfg := readConfig(t, home)
	if !strings.Contains(cfg, `hub: "`+hubURI+`"`) {
		t.Fatalf("config = %q, want hub %q", cfg, hubURI)
	}
	if !strings.Contains(stderr, "Configured hub: "+hubURI) {
		t.Fatalf("stderr = %q, want hub-configured note", stderr)
	}

	// Workspace founded (role founder) and the project adopted.
	if !strings.Contains(cfg, `role: "founder"`) {
		t.Fatalf("config = %q, want role founder", cfg)
	}
	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	projects, err := store.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 {
		t.Fatalf("adopted %d project(s), want 1 (up --scan should adopt work/proj)", len(projects))
	}

	// Sync completed: the empty hub was founded (epoch 1) and the hub file exists
	// with events pushed under the fleet key.
	raw, err := os.ReadFile(hubPath)
	if err != nil {
		t.Fatalf("hub file not written: %v", err)
	}
	var hubEvents []state.Event
	if err := json.Unmarshal(raw, &hubEvents); err != nil {
		t.Fatalf("parse hub file: %v", err)
	}
	if len(hubEvents) == 0 {
		t.Fatalf("hub carries no events after up's sync")
	}

	if !strings.Contains(stdout, "Workspace up:") || !strings.Contains(stdout, "founded") {
		t.Fatalf("stdout = %q, want the up summary", stdout)
	}
}

// TestUpRequiresHub: --hub is mandatory and its absence is a usage error before
// anything is founded.
func TestUpRequiresHub(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	_, stderr, err := executeForTest("--home", home, "--root", root, "up")
	if err == nil {
		t.Fatalf("up without --hub succeeded, want usage error; stderr = %q", stderr)
	}
	if !strings.Contains(stderr, "--hub is required") {
		t.Fatalf("stderr = %q, want --hub-required message", stderr)
	}
	// Nothing was founded.
	if _, statErr := os.Stat(filepath.Join(home, "state.db")); statErr == nil {
		t.Fatalf("up refused for a missing --hub but still initialized the state db")
	}
}

// TestUpSyncFailureLeavesPriorStepsResumable proves the failure semantics: an
// unreachable hub (r2:// with no endpoint) fails at the SYNC step with sync's
// own unwrapped error, while init + hub config are left in place — a re-run of
// `up` with a good hub then completes from there.
func TestUpSyncFailureLeavesPriorStepsResumable(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("DEVSTRAP_HUB_S3_ENDPOINT", "")
	ctx := context.Background()

	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")

	// r2:// is shape-valid (passes the preflight) but sync cannot resolve it with
	// no endpoint configured — that error surfaces from the sync step.
	_, stderr, err := executeForTest("--home", home, "--root", root, "up", "--hub", "r2://no-such-bucket")
	if err == nil {
		t.Fatalf("up with an unreachable r2 hub succeeded, want failure; stderr = %q", stderr)
	}
	if !strings.Contains(stderr, "no endpoint") {
		t.Fatalf("stderr = %q, want sync's own unwrapped 'no endpoint' error", stderr)
	}
	if !strings.Contains(stderr, "safe to keep") {
		t.Fatalf("stderr = %q, want the prior-steps-safe note", stderr)
	}
	// Prior steps landed: config.yaml exists with the (bad) hub and a founder role.
	cfg := readConfig(t, home)
	if !strings.Contains(cfg, `role: "founder"`) || !strings.Contains(cfg, "r2://no-such-bucket") {
		t.Fatalf("config = %q, want founder role + the configured hub after the partial up", cfg)
	}
	// But no epoch was minted (founding happens inside the sync that failed).
	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	epochs, err := store.HeldKeyEpochs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	closeStore(store)
	if len(epochs) != 0 {
		t.Fatalf("held epochs = %v, want none (sync failed before founding)", epochs)
	}

	// Re-run with a good file hub: prior init is reused, the hub is rewritten, and
	// the sync now founds the workspace.
	hubPath := filepath.Join(t.TempDir(), "hub.json")
	hubURI := "file:" + hubPath
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "up", "--hub", hubURI)
	if err != nil {
		t.Fatalf("re-run up: %v\nstderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, "Workspace up:") {
		t.Fatalf("re-run stdout = %q, want the up summary", stdout)
	}
	store2, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store2)
	epochs2, err := store2.HeldKeyEpochs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(epochs2) == 0 {
		t.Fatalf("re-run held epochs = %v, want epoch 1 founded", epochs2)
	}
}
