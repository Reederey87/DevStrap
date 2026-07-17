package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/config"
	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
)

// TestVerifyBlobContentHash (SEC-03): a blob fetched from the hub must hash to
// the sha256 embedded in its signed age_blob:<sha256> ref. A mismatch (hub
// substitution/tampering) is rejected; a missing or malformed hash is rejected.
func TestVerifyBlobContentHash(t *testing.T) {
	payload := []byte("encrypted-blob-content")
	sum := sha256.Sum256(payload)
	hash := hex.EncodeToString(sum[:])
	ref := "age_blob:" + hash

	if err := verifyBlobContentHash(ref, payload); err != nil {
		t.Fatalf("matching hash: unexpected error %v", err)
	}
	if err := verifyBlobContentHash(ref, []byte("tampered-by-hub")); err == nil {
		t.Fatal("mismatched hash: want error, got nil (SEC-03 tamper detection)")
	}
	if err := verifyBlobContentHash("age_blob:", payload); err == nil {
		t.Fatal("empty hash: want error, got nil")
	}
	if err := verifyBlobContentHash("not-a-blob-ref", payload); err == nil {
		t.Fatal("malformed ref: want error, got nil")
	}
}

func TestBlobRefFromEventEnvProfile(t *testing.T) {
	raw, err := json.Marshal(dssync.EnvProfilePayload{
		Path:     "work/acme/api",
		Profile:  "default",
		Provider: "devstrap_encrypted",
		Mode:     "hydrate_or_runtime",
		BlobRef:  "age_blob:" + hex64a,
		VarNames: []string{"API_TOKEN"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ref, ok := blobRefFromEvent(state.Event{Type: dssync.EventEnvProfileUpdated, PayloadJSON: string(raw)})
	if !ok || ref != "age_blob:"+hex64a {
		t.Fatalf("blobRefFromEvent encrypted = (%q, %t), want env ref", ref, ok)
	}
	providerRaw, err := json.Marshal(dssync.EnvProfilePayload{
		Path:     "work/acme/api",
		Profile:  "default",
		Provider: "1password",
		Mode:     "runtime_only",
		Refs:     map[string]string{"API_TOKEN": "op://vault/item/token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ref, ok = blobRefFromEvent(state.Event{Type: dssync.EventEnvProfileUpdated, PayloadJSON: string(providerRaw)})
	if ok || ref != "" {
		t.Fatalf("blobRefFromEvent provider = (%q, %t), want no ref", ref, ok)
	}
}

func TestPushReferencedBlobsPushesMultipleBlobs(t *testing.T) {
	ctx := context.Background()
	paths := config.Paths{Home: t.TempDir(), Root: t.TempDir()}
	hub := dssync.FileHub{Path: filepath.Join(t.TempDir(), "hub.json")}
	refs := []string{
		writeTestBlob(t, paths, []byte("encrypted-draft-one")),
		writeTestBlob(t, paths, []byte("encrypted-draft-two")),
		writeTestBlob(t, paths, []byte("encrypted-draft-three")),
	}

	if err := pushReferencedBlobs(ctx, hub, []state.Event{
		draftSnapshotEvent(t, "evt_001", refs[0]),
		draftSnapshotEvent(t, "evt_002", refs[1]),
		draftSnapshotEvent(t, "evt_003", refs[2]),
	}, paths); err != nil {
		t.Fatalf("pushReferencedBlobs: %v", err)
	}
	for _, ref := range refs {
		rc, err := hub.GetBlob(ctx, blobHashHex(ref))
		if err != nil {
			t.Fatalf("GetBlob(%s): %v", ref, err)
		}
		got, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("read blob %s: %v", ref, err)
		}
		want, err := readEnvBlob(paths, ref)
		if err != nil {
			t.Fatalf("read local blob %s: %v", ref, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("hub blob %s = %q, want %q", ref, got, want)
		}
	}
}

func TestPushReferencedBlobsFailureSurfaces(t *testing.T) {
	ctx := context.Background()
	paths := config.Paths{Home: t.TempDir(), Root: t.TempDir()}
	base := dssync.FileHub{Path: filepath.Join(t.TempDir(), "hub.json")}
	refs := []string{
		writeTestBlob(t, paths, []byte("encrypted-draft-one")),
		writeTestBlob(t, paths, []byte("encrypted-draft-two")),
	}
	hub := failPutBlobHub{Hub: base, failHash: blobHashHex(refs[1]), err: errors.New("forced blob put failure")}

	err := pushReferencedBlobs(ctx, hub, []state.Event{
		draftSnapshotEvent(t, "evt_001", refs[0]),
		draftSnapshotEvent(t, "evt_002", refs[1]),
	}, paths)
	if err == nil {
		t.Fatal("pushReferencedBlobs: want failure, got nil")
	}
	wantPrefix := fmt.Sprintf("push blob %s: forced blob put failure", refs[1])
	if !strings.Contains(err.Error(), wantPrefix) {
		t.Fatalf("pushReferencedBlobs error = %v, want prefix %q", err, wantPrefix)
	}
}

type failPutBlobHub struct {
	dssync.Hub
	failHash string
	err      error
}

func (h failPutBlobHub) PutBlob(ctx context.Context, hash string, r io.Reader) error {
	if hash == h.failHash {
		return h.err
	}
	return h.Hub.PutBlob(ctx, hash, r)
}

func writeTestBlob(t *testing.T, paths config.Paths, ciphertext []byte) string {
	t.Helper()
	sum := sha256.Sum256(ciphertext)
	ref := "age_blob:" + hex.EncodeToString(sum[:])
	if err := writeEnvBlob(paths, ref, ciphertext); err != nil {
		t.Fatalf("writeEnvBlob(%s): %v", ref, err)
	}
	return ref
}

func draftSnapshotEvent(t *testing.T, id, ref string) state.Event {
	t.Helper()
	payload, err := json.Marshal(dssync.DraftSnapshotPayload{
		Path:      "drafts/" + id,
		BlobRef:   ref,
		ByteSize:  1,
		FileCount: 1,
	})
	if err != nil {
		t.Fatalf("marshal draft payload: %v", err)
	}
	return state.Event{
		ID:          id,
		DeviceID:    "dev_a",
		HLC:         100,
		Seq:         1,
		Type:        dssync.EventDraftSnapshotCreated,
		PayloadJSON: string(payload),
		ContentHash: state.ContentHash(string(payload)),
	}
}

// TestSyncCapturesGitstate pins P7-GITSTATE-01: `devstrap sync` captures this
// device's working-state observation for an already-materialized git_repo
// project and mirrors it into device_gitstate (the read side status
// --all-devices/doctor use), not just onto the outbound event log.
func TestSyncCapturesGitstate(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init: %v (%s)", err, stderr)
	}

	nsPath := "work/acme/api"
	repoPath := filepath.Join(root, filepath.FromSlash(nsPath))
	runGit(t, repoPath, "init", "-b", "main")
	runGit(t, repoPath, "config", "user.email", "devstrap@example.test")
	runGit(t, repoPath, "config", "user.name", "DevStrap Test")
	runGit(t, repoPath, "commit", "--allow-empty", "-m", "init")

	opts := testOptions(home, root)
	ctx := context.Background()
	store, err := opts.openState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertProject(ctx, state.UpsertProjectParams{
		Path:                 nsPath,
		Type:                 "git_repo",
		RemoteURL:            "https://github.com/acme/api.git",
		RemoteKey:            "github.com/acme/api",
		LocalPath:            repoPath,
		MaterializationState: "available",
		DirtyState:           "clean",
	}); err != nil {
		t.Fatal(err)
	}
	device, err := store.CurrentDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	closeStore(store)

	hubPath := filepath.Join(t.TempDir(), "hub.json")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "sync", "--hub-file", hubPath, "--namespace-only"); err != nil {
		t.Fatalf("sync: %v (%s)", err, stderr)
	}

	store = openTestStore(t, home)
	rows, err := store.DeviceGitstateForProject(ctx, nsPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("DeviceGitstateForProject(%q) = %d rows, want 1 (rows=%+v)", nsPath, len(rows), rows)
	}
	got := rows[0]
	if got.DeviceID != device.ID {
		t.Fatalf("device_id = %q, want %q (this device's own observation)", got.DeviceID, device.ID)
	}
	if got.Branch != "main" {
		t.Fatalf("branch = %q, want main", got.Branch)
	}
	if got.HeadSHA == "" {
		t.Fatal("head_sha empty, want the commit's SHA")
	}

	// A second sync cycle must not error — UpsertDeviceGitstateTx's
	// HLC-guarded upsert must stay idempotent on repeated capture.
	if _, stderr, err := executeForTest("--home", home, "--root", root, "sync", "--hub-file", hubPath, "--namespace-only"); err != nil {
		t.Fatalf("second sync: %v (%s)", err, stderr)
	}
	rows, err = store.DeviceGitstateForProject(ctx, nsPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("after second sync: DeviceGitstateForProject(%q) = %d rows, want 1", nsPath, len(rows))
	}
}

// TestSyncGitstateCaptureFailureWarnsWithoutFailingCycle pins the P4-GIT-07
// best-effort contract: a project whose local path is not (or no longer) a
// git repository must not fail the sync cycle — it records a scrubbed
// warning on the project's device_project_state row instead.
func TestSyncGitstateCaptureFailureWarnsWithoutFailingCycle(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init: %v (%s)", err, stderr)
	}

	nsPath := "work/acme/broken"
	// A real directory that is NOT a git repository: CaptureGitstate's
	// `git status` fails deterministically without a .git.
	notARepo := t.TempDir()

	opts := testOptions(home, root)
	ctx := context.Background()
	store, err := opts.openState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertProject(ctx, state.UpsertProjectParams{
		Path:                 nsPath,
		Type:                 "git_repo",
		RemoteURL:            "https://github.com/acme/broken.git",
		RemoteKey:            "github.com/acme/broken",
		LocalPath:            notARepo,
		MaterializationState: "available",
		DirtyState:           "clean",
	}); err != nil {
		t.Fatal(err)
	}
	closeStore(store)

	hubPath := filepath.Join(t.TempDir(), "hub.json")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "sync", "--hub-file", hubPath, "--namespace-only"); err != nil {
		t.Fatalf("sync with a broken gitstate capture must still succeed: %v (%s)", err, stderr)
	}

	store = openTestStore(t, home)
	project, err := store.ProjectByPath(ctx, nsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(project.LastError, "gitstate capture: ") {
		t.Fatalf("last_error = %q, want a gitstate capture: warning", project.LastError)
	}
	rows, err := store.DeviceGitstateForProject(ctx, nsPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("DeviceGitstateForProject(%q) = %d rows, want 0 (capture failed, nothing to mirror)", nsPath, len(rows))
	}
}
