package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/platform"
)

// P5-CLI-01 part B: hub * domain --json shapes via the shared opts.render seam.

func TestHubInitJSON(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	hubURI := "git+file://" + filepath.Join(t.TempDir(), "hub.git")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}

	stdout, stderr, err := executeForTest("--home", home, "--json", "hub", "init", "--no-probe", hubURI)
	if err != nil {
		t.Fatalf("hub init --json stderr = %q err = %v", stderr, err)
	}
	var got hubInitResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("hub init --json is not hubInitResult: %v\n%s", err, stdout)
	}
	if got.Hub != hubURI || got.Scheme != "git+file" || got.AlreadyConfigured {
		t.Fatalf("hub init --json = %+v, want hub=%s scheme=git+file already_configured=false", got, hubURI)
	}

	// Re-run same URI: already_configured=true.
	stdout, stderr, err = executeForTest("--home", home, "--json", "hub", "init", "--no-probe", hubURI)
	if err != nil {
		t.Fatalf("hub init --json re-run stderr = %q err = %v", stderr, err)
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("hub init --json re-run: %v\n%s", err, stdout)
	}
	if !got.AlreadyConfigured || got.Hub != hubURI {
		t.Fatalf("hub init --json re-run = %+v, want already_configured=true", got)
	}
}

func TestHubLoginLogoutJSON(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}

	stdout, stderr, err := executeForTestWithStdin(strings.NewReader("login-secret-json\n"),
		"--home", home, "--root", root, "--json", "hub", "login", "--access-key-id", "AKIAJSON")
	if err != nil {
		t.Fatalf("hub login --json stderr = %q err = %v", stderr, err)
	}
	if strings.Contains(stdout+stderr, "login-secret-json") {
		t.Fatalf("hub login --json leaked the secret: stdout=%q stderr=%q", stdout, stderr)
	}
	var login hubLoginResult
	if err := json.Unmarshal([]byte(stdout), &login); err != nil {
		t.Fatalf("hub login --json is not hubLoginResult: %v\n%s", err, stdout)
	}
	if login.Action != "stored" || login.WorkspaceID == "" || login.Location == "" {
		t.Fatalf("hub login --json = %+v, want action=stored with workspace_id and location", login)
	}

	stdout, stderr, err = executeForTest("--home", home, "--root", root, "--json", "hub", "logout")
	if err != nil {
		t.Fatalf("hub logout --json stderr = %q err = %v", stderr, err)
	}
	var logout hubLogoutResult
	if err := json.Unmarshal([]byte(stdout), &logout); err != nil {
		t.Fatalf("hub logout --json is not hubLogoutResult: %v\n%s", err, stdout)
	}
	if logout.Action != "removed" || logout.WorkspaceID != login.WorkspaceID {
		t.Fatalf("hub logout --json = %+v, want action=removed workspace_id=%s", logout, login.WorkspaceID)
	}
}

func TestHubGCJSON(t *testing.T) {
	env, store, _ := setupCompact(t)
	defer closeStore(store)
	home := env.opts.v.GetString("home")
	root := env.opts.v.GetString("root")

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json",
		"hub", "gc", "--hub-file", env.hubPath, "--grace-window", "0")
	if err != nil {
		t.Fatalf("hub gc --json stderr = %q err = %v", stderr, err)
	}
	var got hubGCResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("hub gc --json is not hubGCResult: %v\n%s", err, stdout)
	}
	if got.GraceWindow == "" || got.Keep < 1 {
		t.Fatalf("hub gc --json = %+v, want grace_window and keep set", got)
	}
	// Empty file hub: nothing deleted; shape is what we pin.
	if got.BlobsDeleted < 0 || got.BlobsRetained < 0 || got.PrunedSnapshots < 0 {
		t.Fatalf("hub gc --json = %+v, want non-negative counts", got)
	}
}

func TestHubCompactJSON(t *testing.T) {
	env, store, _ := setupCompact(t)
	defer closeStore(store)

	env.opts.v.Set("json", true)
	var out bytes.Buffer
	if err := hubCompact(env.ctx, &out, io.Discard, env.opts, store, env.hub(t, store), env.hubID, env.paths, 2, 0, true, false); err != nil {
		t.Fatalf("hubCompact --json: %v", err)
	}
	var got hubCompactResult
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("hub compact --json is not hubCompactResult: %v\n%s", err, out.String())
	}
	if got.DryRun {
		t.Fatalf("hub compact --json dry_run=true, want false")
	}
	if got.SnapshotSHA == "" {
		t.Fatalf("hub compact --json missing snapshot_sha: %+v", got)
	}
	if len(got.Floors) == 0 {
		t.Fatalf("hub compact --json floors empty: %+v", got)
	}
	if got.KeepSnapshots != 2 {
		t.Fatalf("hub compact --json keep_snapshots = %d, want 2", got.KeepSnapshots)
	}
}

// TestHubCompactJSONStaysPureWhenDrainingBlobs is a reviewer-caught regression:
// pushLocalEventsGated (called from hubCompact's push phase) can write an
// informational "Removed N superseded blob(s) from the hub" line when a
// prior local-only revoke's queued delete finally drains. Before this test,
// hubCompact routed that call through `stdout`, so under --json a human line
// would precede the JSON object and corrupt the parseable-single-document
// contract. Verify stdout is ONLY the JSON object and the informational line
// (if it fires) lands on stderr instead.
func TestHubCompactJSONStaysPureWhenDrainingBlobs(t *testing.T) {
	env, store, _ := setupCompact(t)
	defer closeStore(store)

	hub := env.hub(t, store)
	// Seed a blob under the hub's real content-address naming, then queue it
	// for deletion the way a local-only revoke's rewrap path does (P5-PROD-02).
	// This ref is not referenced by any env binding or draft snapshot, so
	// drainPendingHubDeletes will delete it and report deleted=1 — pushed
	// through hubCompact's push phase as pushLocalEventsGated's "Removed 1
	// superseded blob(s)…" line.
	if err := hub.PutBlob(env.ctx, hex64a, strings.NewReader("old-ciphertext")); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	if err := store.QueuePendingHubDelete(env.ctx, "age_blob:"+hex64a); err != nil {
		t.Fatalf("QueuePendingHubDelete: %v", err)
	}

	env.opts.v.Set("json", true)
	var stdout, stderr bytes.Buffer
	if err := hubCompact(env.ctx, &stdout, &stderr, env.opts, store, hub, env.hubID, env.paths, 2, 0, true, false); err != nil {
		t.Fatalf("hubCompact --json: %v", err)
	}
	var got hubCompactResult
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("hub compact --json stdout is not a single pure hubCompactResult document (drain-blob message likely leaked onto stdout): %v\nstdout=%q", err, stdout.String())
	}
	if !strings.Contains(stderr.String(), "Removed 1 superseded blob(s) from the hub") {
		t.Fatalf("expected the drain notice on stderr, got stderr=%q", stderr.String())
	}
	if refs, _ := store.PendingHubDeletes(env.ctx); len(refs) != 0 {
		t.Fatalf("pending hub delete queue not drained: %v", refs)
	}
}

func TestHubCompactDryRunJSON(t *testing.T) {
	env, store, _ := setupCompact(t)
	defer closeStore(store)

	env.opts.v.Set("json", true)
	var out bytes.Buffer
	if err := hubCompact(env.ctx, &out, io.Discard, env.opts, store, env.hub(t, store), env.hubID, env.paths, 2, 0, true, true); err != nil {
		t.Fatalf("hubCompact --dry-run --json: %v", err)
	}
	var got hubCompactResult
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("hub compact --dry-run --json is not hubCompactResult: %v\n%s", err, out.String())
	}
	if !got.DryRun {
		t.Fatalf("hub compact dry-run --json dry_run=false, want true")
	}
	if got.SnapshotSHA != "" {
		t.Fatalf("dry-run should not report a written snapshot_sha, got %q", got.SnapshotSHA)
	}
	if got.Floors == nil {
		t.Fatalf("hub compact dry-run --json missing floors")
	}
	if got.SnapshotEntries < 1 {
		t.Fatalf("hub compact dry-run --json snapshot_entries = %d, want >=1", got.SnapshotEntries)
	}
	if got.KeepSnapshots != 2 {
		t.Fatalf("hub compact dry-run --json keep_snapshots = %d, want 2", got.KeepSnapshots)
	}
}

func TestHubMigrateEventsJSON(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	hubFile := filepath.Join(t.TempDir(), "hub.json")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}

	// File-backed hubs never used the legacy layout — migrate-events is a no-op.
	stdout, stderr, err := executeForTest("--home", home, "--json", "hub", "migrate-events", "--hub-file", hubFile)
	if err != nil {
		t.Fatalf("hub migrate-events --json stderr = %q err = %v", stderr, err)
	}
	var got hubMigrateEventsResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("hub migrate-events --json is not hubMigrateEventsResult: %v\n%s", err, stdout)
	}
	if got.Migrated != 0 || got.UnparseableKept != 0 || got.AlreadyMigrated != 0 || got.DryRun {
		t.Fatalf("hub migrate-events --json = %+v, want zero counts and dry_run=false", got)
	}

	stdout, stderr, err = executeForTest("--home", home, "--json", "hub", "migrate-events", "--hub-file", hubFile, "--dry-run")
	if err != nil {
		t.Fatalf("hub migrate-events --dry-run --json stderr = %q err = %v", stderr, err)
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("hub migrate-events --dry-run --json: %v\n%s", err, stdout)
	}
	if !got.DryRun || got.Migrated != 0 || got.UnparseableKept != 0 {
		t.Fatalf("hub migrate-events dry-run --json = %+v, want dry_run=true zero counts", got)
	}
}
