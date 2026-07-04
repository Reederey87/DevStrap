package sync

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
)

func testFileHub(t *testing.T) FileHub {
	t.Helper()
	return FileHub{Path: filepath.Join(t.TempDir(), "hub.json")}
}

func manifestBytes(t *testing.T, floors map[string]int64) []byte {
	t.Helper()
	raw, err := json.Marshal(RetentionManifest{V: snapshotVersion, WorkspaceID: "ws_test", Floors: floors})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestFileHubRetentionAbsentMeansNoFloor(t *testing.T) {
	ctx := context.Background()
	h := testFileHub(t)
	if _, _, err := h.GetRetention(ctx); !errors.Is(err, ErrRetentionNotFound) {
		t.Fatalf("got %v, want ErrRetentionNotFound", err)
	}
	if err := h.Push(ctx, []state.Event{makeTestEvent("e1", "dev_a", 10, 1)}); err != nil {
		t.Fatal(err)
	}
	events, err := h.Pull(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("pull without manifest must deliver everything, got %d", len(events))
	}
}

func TestFileHubRetentionCAS(t *testing.T) {
	ctx := context.Background()
	h := testFileHub(t)
	// Create requires no existing manifest.
	if err := h.PutRetention(ctx, manifestBytes(t, map[string]int64{"dev_a": 2}), ""); err != nil {
		t.Fatal(err)
	}
	// A second create loses.
	if err := h.PutRetention(ctx, manifestBytes(t, map[string]int64{"dev_a": 3}), ""); !errors.Is(err, ErrRetentionConflict) {
		t.Fatalf("second create: got %v, want ErrRetentionConflict", err)
	}
	raw, etag, err := h.GetRetention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if floors, _ := ParseRetentionFloors(raw); floors["dev_a"] != 2 {
		t.Fatalf("unexpected floors after failed CAS: %v", floors)
	}
	// Update with the current etag wins; with a stale one loses.
	if err := h.PutRetention(ctx, manifestBytes(t, map[string]int64{"dev_a": 4}), etag); err != nil {
		t.Fatal(err)
	}
	if err := h.PutRetention(ctx, manifestBytes(t, map[string]int64{"dev_a": 5}), etag); !errors.Is(err, ErrRetentionConflict) {
		t.Fatalf("stale etag: got %v, want ErrRetentionConflict", err)
	}
}

func TestFileHubPullHonorsManifestFloor(t *testing.T) {
	ctx := context.Background()
	h := testFileHub(t)
	if err := h.Push(ctx, []state.Event{
		makeTestEvent("e1", "dev_a", 10, 1),
		makeTestEvent("e2", "dev_a", 20, 2),
		makeTestEvent("e3", "dev_a", 30, 3),
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.PutRetention(ctx, manifestBytes(t, map[string]int64{"dev_a": 3}), ""); err != nil {
		t.Fatal(err)
	}
	// Cursor at 2: after+1 == 3 == floor — satisfied, incremental pull works.
	if _, err := h.Pull(ctx, Cursor{"dev_a": 2}); err != nil {
		t.Fatalf("cursor at floor boundary must pull incrementally: %v", err)
	}
	// Cursor at 1: after+1 == 2 < 3 — gap below the floor, snapshot required.
	if _, err := h.Pull(ctx, Cursor{"dev_a": 1}); !errors.Is(err, ErrSnapshotRequired) {
		t.Fatalf("cursor below floor: got %v, want ErrSnapshotRequired", err)
	}
	// A fresh device (no cursor) is below the floor too — bootstrap via snapshot.
	if _, err := h.Pull(ctx, nil); !errors.Is(err, ErrSnapshotRequired) {
		t.Fatalf("fresh cursor below floor: got %v, want ErrSnapshotRequired", err)
	}
}

func TestFileHubPullFailsClosedOnGarbledManifest(t *testing.T) {
	ctx := context.Background()
	h := testFileHub(t)
	if err := h.PutRetention(ctx, []byte("not json"), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pull(ctx, nil); err == nil || errors.Is(err, ErrSnapshotRequired) {
		t.Fatalf("garbled manifest must be a hard error, got %v", err)
	}
}

func TestFileHubSnapshotObjectRoundTrip(t *testing.T) {
	ctx := context.Background()
	h := testFileHub(t)
	wck, _ := NewWCK()
	obj, sha, err := SealSnapshot(testSnapshot(), wck, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.PutSnapshotObject(ctx, sha, obj); err != nil {
		t.Fatal(err)
	}
	if err := h.PutSnapshotObject(ctx, sha, obj); err != nil {
		t.Fatalf("content-addressed re-put must be a no-op: %v", err)
	}
	got, err := h.GetSnapshotObject(ctx, sha)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(obj) {
		t.Fatal("snapshot object bytes did not round-trip")
	}
	list, err := h.ListSnapshotObjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Key != sha {
		t.Fatalf("list: %+v", list)
	}
	if err := h.DeleteSnapshotObject(ctx, sha); err != nil {
		t.Fatal(err)
	}
	if err := h.DeleteSnapshotObject(ctx, sha); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
	if _, err := h.GetSnapshotObject(ctx, sha); !errors.Is(err, ErrBlobNotFound) {
		t.Fatalf("got %v, want ErrBlobNotFound", err)
	}
}

func TestFileHubCompactEventsBelow(t *testing.T) {
	ctx := context.Background()
	h := testFileHub(t)
	if err := h.Push(ctx, []state.Event{
		makeTestEvent("a1", "dev_a", 10, 1),
		makeTestEvent("a2", "dev_a", 20, 2),
		makeTestEvent("a3", "dev_a", 30, 3),
		makeTestEvent("b1", "dev_b", 15, 1),
		{ID: "legacy", DeviceID: "dev_a", HLC: 5, Seq: 0, Type: "project.added", PayloadJSON: "{}"},
	}); err != nil {
		t.Fatal(err)
	}
	deleted, err := h.CompactEventsBelow(ctx, Cursor{"dev_a": 3})
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2 (a1, a2)", deleted)
	}
	events, err := h.Pull(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, e := range events {
		ids[e.ID] = true
	}
	// a3 is AT the floor (retained), dev_b untouched, Seq<=0 never compacted.
	for _, want := range []string{"a3", "b1", "legacy"} {
		if !ids[want] {
			t.Errorf("event %s missing after compaction", want)
		}
	}
	if ids["a1"] || ids["a2"] {
		t.Errorf("below-floor events survived compaction: %v", ids)
	}
}

func makeTestEvent(id, dev string, hlc, seq int64) state.Event {
	payload := `{"path":"work/` + id + `"}`
	return state.Event{
		ID:          id,
		DeviceID:    dev,
		HLC:         hlc,
		Seq:         seq,
		Type:        "project.added",
		PayloadJSON: payload,
		ContentHash: state.ContentHash(payload),
	}
}
