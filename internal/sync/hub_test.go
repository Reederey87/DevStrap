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

// TestFileHubPullInclusiveBoundaryDeliversSameHLC (HUB-13): the pull boundary
// is inclusive (>=) so a same-HLC event from another device that arrives AFTER
// the cursor was advanced to that HLC is still delivered on the next pull. With
// a strict > boundary it would be silently dropped — a lost namespace event.
// Re-delivering the boundary is safe because ApplyEvents dedups by event ID.
func TestFileHubPullInclusiveBoundaryDeliversSameHLC(t *testing.T) {
	ctx := context.Background()
	hub := FileHub{Path: filepath.Join(t.TempDir(), "hub.json")}
	const hlc = int64(1000)
	a := state.Event{ID: "evt-a", DeviceID: "dev-a", HLC: hlc, Type: "project.added", PayloadJSON: "{}", ContentHash: "sha256:a"}
	b := state.Event{ID: "evt-b", DeviceID: "dev-b", HLC: hlc, Type: "project.added", PayloadJSON: "{}", ContentHash: "sha256:b"}
	if err := hub.Push(ctx, []state.Event{a}); err != nil {
		t.Fatal(err)
	}
	// First pull from 0 delivers A; the cursor would advance to hlc.
	first, err := hub.Pull(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].ID != "evt-a" {
		t.Fatalf("first pull = %+v, want only evt-a", first)
	}
	// B arrives at the same HLC after the cursor was set to hlc.
	if err := hub.Push(ctx, []state.Event{b}); err != nil {
		t.Fatal(err)
	}
	// HUB-13: pull AT the cursor (hlc) must still deliver B (inclusive >=).
	// A is re-delivered too (boundary overlap) and would be deduped by
	// ApplyEvents; the critical assertion is that B is NOT dropped.
	second, err := hub.Pull(ctx, hlc)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range second {
		if e.ID == "evt-b" {
			found = true
		}
	}
	if !found {
		t.Fatalf("second pull at cursor = %v, want evt-b included (inclusive boundary)", eventIDs(second))
	}
}

func eventIDs(events []state.Event) []string {
	ids := make([]string, len(events))
	for i, e := range events {
		ids[i] = e.ID
	}
	return ids
}
