package sync

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
)

// TestFileHubBlobPutGetDelete (SEC-01/HUB-12): the file-backed test hub supports
// the blob reclamation primitive — DeleteBlob removes a content-addressed blob
// and is idempotent on a missing blob.
func TestFileHubBlobPutGetDelete(t *testing.T) {
	ctx := context.Background()
	hub := FileHub{Path: filepath.Join(t.TempDir(), "hub.json")}
	hash := strings.Repeat("a", 64)
	data := []byte("encrypted-blob-content")

	if err := hub.PutBlob(ctx, hash, bytes.NewReader(data)); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	rc, err := hub.GetBlob(ctx, hash)
	if err != nil {
		t.Fatalf("GetBlob: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, data) {
		t.Fatalf("GetBlob = %q, want %q", got, data)
	}
	listed, err := hub.ListBlobs(ctx)
	if err != nil {
		t.Fatalf("ListBlobs: %v", err)
	}
	if len(listed) != 1 || listed[0].Key != hash {
		t.Fatalf("ListBlobs = %+v, want key %s", listed, hash)
	}
	if listed[0].LastModified.IsZero() {
		t.Fatal("ListBlobs returned zero LastModified")
	}

	if err := hub.DeleteBlob(ctx, hash); err != nil {
		t.Fatalf("DeleteBlob: %v", err)
	}
	if _, err := hub.GetBlob(ctx, hash); !errors.Is(err, ErrBlobNotFound) {
		t.Fatalf("GetBlob after delete = %v, want ErrBlobNotFound", err)
	}
	// Idempotent: deleting a missing blob is not an error.
	if err := hub.DeleteBlob(ctx, hash); err != nil {
		t.Fatalf("idempotent delete of missing blob: %v", err)
	}
	// Invalid key is rejected.
	if err := hub.DeleteBlob(ctx, "short"); err == nil {
		t.Fatal("expected error for invalid blob key")
	}
}

// TestFileHubDeleteBlobLeavesEventLogUntouched ensures DeleteBlob only touches
// the blob directory, not the event log file.
func TestFileHubDeleteBlobLeavesEventLogUntouched(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	hub := FileHub{Path: filepath.Join(dir, "hub.json")}
	hash := strings.Repeat("a", 64)
	if err := hub.PutBlob(ctx, hash, bytes.NewReader([]byte("blob"))); err != nil {
		t.Fatal(err)
	}
	if err := hub.DeleteBlob(ctx, hash); err != nil {
		t.Fatal(err)
	}
	// The blob dir should now be empty (or absent) but no panic/error.
	if entries, err := os.ReadDir(hub.blobDir()); err == nil && len(entries) != 0 {
		t.Fatalf("blob dir not empty after delete: %v", entries)
	}
}

func TestFileHubHasEvents(t *testing.T) {
	ctx := context.Background()
	hub := FileHub{Path: filepath.Join(t.TempDir(), "hub.json")}

	hasEvents, err := hub.HasEvents(ctx)
	if err != nil {
		t.Fatalf("HasEvents fresh: %v", err)
	}
	if hasEvents {
		t.Fatal("HasEvents fresh = true, want false")
	}

	event := state.Event{ID: "evt-a", DeviceID: "dev-a", HLC: 100, Type: "project.added", PayloadJSON: "{}", ContentHash: "sha256:a"}
	if err := hub.Push(ctx, []state.Event{event}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	hasEvents, err = hub.HasEvents(ctx)
	if err != nil {
		t.Fatalf("HasEvents populated: %v", err)
	}
	if !hasEvents {
		t.Fatal("HasEvents populated = false, want true")
	}
}

// TestFileHubPullLateArrivalDelivered (P5-SYNC-01, retiring HUB-13): an event
// from another device that lands on the hub AFTER this device's view has moved
// on — same HLC, or an arbitrarily OLDER one (the "offline device forgot to
// push" case the HLC watermark permanently stranded) — is still delivered,
// because each origin device's stream has its own Seq cursor. The boundary is
// exact: the consumed device's own events are NOT re-delivered (no HUB-13
// overlap, zero re-delivery).
func TestFileHubPullLateArrivalDelivered(t *testing.T) {
	ctx := context.Background()
	hub := FileHub{Path: filepath.Join(t.TempDir(), "hub.json")}
	a := state.Event{ID: "evt-a", DeviceID: "dev-a", Seq: 1, HLC: 1000, Type: "project.added", PayloadJSON: "{}", ContentHash: "sha256:a"}
	// B's event carries a far OLDER HLC (queued while offline) and lands late.
	b := state.Event{ID: "evt-b", DeviceID: "dev-b", Seq: 1, HLC: 10, Type: "project.added", PayloadJSON: "{}", ContentHash: "sha256:b"}
	if err := hub.Push(ctx, []state.Event{a}); err != nil {
		t.Fatal(err)
	}
	first, err := hub.Pull(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].ID != "evt-a" {
		t.Fatalf("first pull = %+v, want only evt-a", first)
	}
	// The puller consumed dev-a through seq 1; THEN B's old event arrives.
	if err := hub.Push(ctx, []state.Event{b}); err != nil {
		t.Fatal(err)
	}
	second, err := hub.Pull(ctx, Cursor{"dev-a": 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].ID != "evt-b" {
		t.Fatalf("second pull = %v, want exactly evt-b (late arrival delivered, boundary exact)", eventIDs(second))
	}
}

// TestFileHubPullRetentionFloorPerDevice (P5-HUB-03, Seq-re-based): a cursor
// below any device's retention floor forces a snapshot exchange.
func TestFileHubPullRetentionFloorPerDevice(t *testing.T) {
	ctx := context.Background()
	hub := FileHub{
		Path:          filepath.Join(t.TempDir(), "hub.json"),
		RetentionSeqs: map[string]int64{"dev-a": 5},
	}
	if _, err := hub.Pull(ctx, Cursor{"dev-a": 2}); !errors.Is(err, ErrSnapshotRequired) {
		t.Fatalf("Pull below floor = %v, want ErrSnapshotRequired", err)
	}
	if _, err := hub.Pull(ctx, Cursor{"dev-a": 4}); err != nil {
		t.Fatalf("Pull at floor boundary: %v", err)
	}
}

func eventIDs(events []state.Event) []string {
	ids := make([]string, len(events))
	for i, e := range events {
		ids[i] = e.ID
	}
	return ids
}
