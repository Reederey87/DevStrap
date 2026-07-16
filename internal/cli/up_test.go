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

// TestUpScanRetryDoesNotDuplicateAdoption proves the fix for a review finding
// (PR #202): `up`'s default --scan used to call the one-shot adoptFindings on
// every retry, which re-stamps a FRESH project.added event for an
// already-adopted project even though the project ROW itself is only upserted
// (so a naive project-count check wouldn't catch it). `up` now uses the same
// idempotent adoptNewFindings path run-loop uses. Re-running `up` after a full
// success must push no additional project.added event for the same project.
func TestUpScanRetryDoesNotDuplicateAdoption(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	ctx := context.Background()

	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	hubPath := filepath.Join(t.TempDir(), "hub.json")
	hubURI := "file:" + hubPath

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

	if _, stderr, err := executeForTest("--home", home, "--root", root, "up", "--hub", hubURI); err != nil {
		t.Fatalf("first up: %v\nstderr=%s", err, stderr)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "up", "--hub", hubURI); err != nil {
		t.Fatalf("second up (retry): %v\nstderr=%s", err, stderr)
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
		t.Fatalf("adopted %d project(s) after retry, want 1 (still de-duplicated)", len(projects))
	}

	// The hub file itself stores envelope-encrypted events (no readable Type),
	// so check this device's own LOCAL plaintext event log instead — the
	// duplicate-re-stamp bug would show here even though the project ROW
	// (checked above) is only ever upserted, never duplicated.
	localEvents, err := store.LocalPendingEventsBySeq(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	added := 0
	for _, ev := range localEvents {
		if ev.Type == "project.added" && strings.Contains(ev.PayloadJSON, `"work/proj"`) {
			added++
		}
	}
	if added != 1 {
		t.Fatalf("local event log carries %d project.added event(s) for the one adopted project after a retry, want exactly 1 (no duplicate re-stamp)", added)
	}
}

// TestUpRefusesExistingJoiner proves the fix for a review finding (PR #202):
// `up` used to proceed silently on a device that already joined an existing
// workspace (role: joiner) — runInit leaves an existing config untouched, the
// P6-SEC-02 founder gate then defers sync without founding anything, yet `up`
// still printed a false "founded" success. `up` now refuses up front.
func TestUpRefusesExistingJoiner(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")

	founderHome := filepath.Join(t.TempDir(), ".devstrap-founder")
	founderRoot := filepath.Join(t.TempDir(), "Code-founder")
	if _, stderr, err := executeForTest("--home", founderHome, "--root", founderRoot, "up", "--hub", "file:"+filepath.Join(t.TempDir(), "hub.json")); err != nil {
		t.Fatalf("founder up: %v\nstderr=%s", err, stderr)
	}
	code, _, err := executeForTest("--home", founderHome, "--root", founderRoot, "devices", "pairing-code")
	if err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "join", pairingLine(t, code)); err != nil {
		t.Fatalf("join: %v\nstderr=%s", err, stderr)
	}
	beforeConfig := readConfig(t, home)

	_, stderr, err := executeForTest("--home", home, "--root", root, "up", "--hub", "file:"+filepath.Join(t.TempDir(), "other-hub.json"))
	if err == nil {
		t.Fatalf("up on an existing joiner succeeded, want refusal; stderr = %q", stderr)
	}
	if !strings.Contains(stderr, "already joined") || !strings.Contains(stderr, "joiner") {
		t.Fatalf("stderr = %q, want a clear already-joined refusal", stderr)
	}
	// The joiner's config must be byte-identical after the refused `up` — a
	// substring check for the rejected hub value alone would miss a refused
	// `up` that cleared or otherwise rewrote unrelated config (review
	// finding, PR #202).
	afterConfig := readConfig(t, home)
	if afterConfig != beforeConfig {
		t.Fatalf("config changed after refused `up`:\nbefore=%q\nafter=%q", beforeConfig, afterConfig)
	}
}

// TestUpRejectsEmptyFileHub proves the fix for a review finding (PR #202): a
// bare "file:" (empty path) used to pass hubConfigured's preflight
// unvalidated, so `up --hub file:` founded the workspace and minted a key
// epoch before the empty path failed downstream in the hub backend. The
// preflight must now refuse it before anything is written.
func TestUpRejectsEmptyFileHub(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")

	_, stderr, err := executeForTest("--home", home, "--root", root, "up", "--hub", "file:")
	if err == nil {
		t.Fatalf("up --hub file: succeeded, want a preflight refusal; stderr = %q", stderr)
	}
	if !strings.Contains(stderr, "must include a path") {
		t.Fatalf("stderr = %q, want the file-hub-needs-a-path message", stderr)
	}
	// The "nothing written before validation" contract covers config.yaml too,
	// not just state.db, and any unexpected Stat error must fail the test
	// rather than being silently treated as "absent" (review finding, PR #202).
	for _, path := range []string{
		filepath.Join(home, "state.db"),
		filepath.Join(home, "config.yaml"),
	} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("up --hub file: was refused but wrote %s (stat error: %v)", path, statErr)
		}
	}
}

// TestUpPositionalRootPropagatesToScanAndSync proves the fix for a review
// finding (PR #202): runInit resolved a positional [root] argument only in
// its own local variable, never writing it back into the shared viper config
// — so `devstrap up /custom/root --hub …` could initialize one root but scan
// and sync a different, stale default root, since up's later steps
// (runLoopScanAdopt, rewriteConfigHub, runSyncCycle) each call opts.paths()
// fresh rather than reusing runInit's local value. This test passes the root
// ONLY as a positional argument (no --root flag) and a pre-populated repo
// under it, so a broken propagation would scan/adopt nothing.
func TestUpPositionalRootPropagatesToScanAndSync(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	ctx := context.Background()

	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	hubPath := filepath.Join(t.TempDir(), "hub.json")
	hubURI := "file:" + hubPath

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

	// No --root flag: root comes ONLY from the positional argument.
	if _, stderr, err := executeForTest("--home", home, "up", root, "--hub", hubURI); err != nil {
		t.Fatalf("up: %v\nstderr=%s", err, stderr)
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
		t.Fatalf("adopted %d project(s), want 1 — the positional root was not propagated from runInit to the later scan step (both read opts.paths(), the same mechanism runSyncCycle/rewriteConfigHub rely on)", len(projects))
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
