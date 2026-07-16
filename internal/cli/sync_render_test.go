package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/state"
)

// P5-CLI-01 part B: sync / run-loop --once --json shapes via the shared
// opts.render seam. run-loop --once reuses syncResult free of charge because
// runLoopTick returns runSyncCycle's result directly.

func TestSyncDryRunJSON(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	hubPath := filepath.Join(t.TempDir(), "hub.json")

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json", "sync", "--hub-file", hubPath, "--dry-run")
	if err != nil {
		t.Fatalf("sync --dry-run --json stderr = %q err = %v", stderr, err)
	}

	var got syncResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("sync --dry-run --json is not syncResult: %v\n%s", err, stdout)
	}
	if !got.DryRun {
		t.Fatalf("dry_run = false, want true")
	}
	if got.HubID != "file:"+hubPath {
		t.Fatalf("hub_id = %q, want file:%s", got.HubID, hubPath)
	}
	if got.WouldPush < 0 {
		t.Fatalf("would_push = %d, want >= 0", got.WouldPush)
	}
}

func TestSyncJSONNamespaceOnly(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	hubPath := filepath.Join(t.TempDir(), "hub.json")

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json", "sync", "--hub-file", hubPath, "--namespace-only")
	if err != nil {
		t.Fatalf("sync --namespace-only --json stderr = %q err = %v", stderr, err)
	}

	var got syncResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("sync --json is not a pure syncResult document: %v\nstdout=%q", err, stdout)
	}
	if !got.NamespaceOnly {
		t.Fatalf("namespace_only = false, want true")
	}
	if got.DryRun {
		t.Fatalf("dry_run set on real cycle")
	}
	if got.HubID != "file:"+hubPath {
		t.Fatalf("hub_id = %q, want file:%s", got.HubID, hubPath)
	}
	// This fixture never adds a project, so a fresh founder's first cycle can
	// legitimately have zero local events to push (Pushed==0, Deferred==false is
	// a valid outcome, not just the deferred-awaiting-grant case) — assert only
	// that the two fields are mutually consistent (deferred implies nothing was
	// pushed), matching pushLocalEventsGated's own documented contract, rather
	// than assuming a push always happens on a bare founder init.
	if got.Deferred && got.Pushed != 0 {
		t.Fatalf("pushed = %d deferred = %v, want pushed=0 when deferred", got.Pushed, got.Deferred)
	}
	if got.Pushed < 0 {
		t.Fatalf("pushed = %d, want >= 0", got.Pushed)
	}
}

func TestRunLoopOnceJSON(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	hubPath := filepath.Join(t.TempDir(), "hub.json")

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json", "run-loop", "--once", "--hub-file", hubPath, "--namespace-only")
	if err != nil {
		t.Fatalf("run-loop --once --json stderr = %q err = %v", stderr, err)
	}

	var got syncResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("run-loop --once --json is not syncResult (should reuse sync's shape): %v\nstdout=%q stderr=%q", err, stdout, stderr)
	}
	if !got.NamespaceOnly {
		t.Fatalf("namespace_only = false, want true")
	}
	if got.HubID != "file:"+hubPath {
		t.Fatalf("hub_id = %q, want file:%s", got.HubID, hubPath)
	}
	// Tick header is progress on stderr, not mixed into stdout.
	if strings.Contains(stdout, "run-loop tick") {
		t.Fatalf("stdout leaked run-loop tick progress: %q", stdout)
	}
}

// TestSyncJSONStaysPureWhenRotationOwedWarns is a purity regression for the
// same class as TestHubCompactJSONStaysPureWhenDrainingBlobs: an owed WCK
// rotation that fails early (malformed recipient) used to Fprintf the loud
// "rotation owed since … still failing" warnings directly to stdout. Under
// --json that would precede the JSON object and break single-document parsing.
// Warnings now land on stderr; stdout is only the syncResult.
func TestSyncJSONStaysPureWhenRotationOwedWarns(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	ctx := t.Context()
	home, root := rotateTestHome(t)
	st := openTestStore(t, home)
	if err := st.UpsertDevice(ctx, state.Device{
		ID: "dev_badrec", Name: "badrec", OS: "linux", Arch: "arm64",
		PublicKey: "not-an-age-recipient", SigningPublicKey: "irrelevant", TrustState: "approved",
	}); err != nil {
		t.Fatal(err)
	}
	if err := markWCKRotationPending(ctx, st, 1); err != nil {
		t.Fatal(err)
	}
	closeStore(st)

	hubPath := filepath.Join(t.TempDir(), "hub.json")
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json", "sync", "--hub-file", hubPath, "--namespace-only")
	if err != nil {
		t.Fatalf("sync --json with owed rotation failure: %v (stdout=%q stderr=%q)", err, stdout, stderr)
	}

	var got syncResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("sync --json stdout is not a single pure syncResult (rotation warning likely leaked onto stdout): %v\nstdout=%q", err, stdout)
	}
	if got.KeyRotated {
		t.Fatalf("key_rotated = true, want false on early owed failure")
	}
	if !strings.Contains(stderr, "rotation owed since") || !strings.Contains(stderr, "still failing") {
		t.Fatalf("expected owed-rotation warning on stderr, got stderr=%q", stderr)
	}
	if strings.Contains(stdout, "warning:") {
		t.Fatalf("stdout still contains human warning text: %q", stdout)
	}
}
