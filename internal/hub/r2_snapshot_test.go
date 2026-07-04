package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
)

func testR2SnapshotHub() (R2Hub, *memS3) {
	m := newMemS3()
	return R2Hub{S3: m, WorkspaceID: "ws_test", Retry: R2Retry{MaxAttempts: 1}}, m
}

func r2ManifestBytes(t *testing.T, floors map[string]int64) []byte {
	t.Helper()
	raw, err := json.Marshal(dssync.RetentionManifest{V: 1, WorkspaceID: "ws_test", Floors: floors})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestR2RetentionAbsentMeansNoFloor(t *testing.T) {
	ctx := context.Background()
	h, _ := testR2SnapshotHub()
	if _, _, err := h.GetRetention(ctx); !errors.Is(err, dssync.ErrRetentionNotFound) {
		t.Fatalf("got %v, want ErrRetentionNotFound", err)
	}
	if err := h.Push(ctx, []state.Event{makeEvent("e1", "dev_a", 10, 1, "project.added", "{}")}); err != nil {
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

func TestR2RetentionCASConflict(t *testing.T) {
	ctx := context.Background()
	h, _ := testR2SnapshotHub()
	if err := h.PutRetention(ctx, r2ManifestBytes(t, map[string]int64{"dev_a": 2}), ""); err != nil {
		t.Fatal(err)
	}
	if err := h.PutRetention(ctx, r2ManifestBytes(t, map[string]int64{"dev_a": 3}), ""); !errors.Is(err, dssync.ErrRetentionConflict) {
		t.Fatalf("second create: got %v, want ErrRetentionConflict", err)
	}
	_, etag, err := h.GetRetention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.PutRetention(ctx, r2ManifestBytes(t, map[string]int64{"dev_a": 4}), etag); err != nil {
		t.Fatal(err)
	}
	if err := h.PutRetention(ctx, r2ManifestBytes(t, map[string]int64{"dev_a": 5}), etag); !errors.Is(err, dssync.ErrRetentionConflict) {
		t.Fatalf("stale etag: got %v, want ErrRetentionConflict", err)
	}
	raw, _, err := h.GetRetention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	floors, err := dssync.ParseRetentionFloors(raw)
	if err != nil {
		t.Fatal(err)
	}
	if floors["dev_a"] != 4 {
		t.Fatalf("floors = %v, want dev_a:4", floors)
	}
}

func TestR2PullHonorsManifestFloor(t *testing.T) {
	ctx := context.Background()
	h, _ := testR2SnapshotHub()
	if err := h.Push(ctx, []state.Event{
		makeEvent("e1", "dev_a", 10, 1, "project.added", "{}"),
		makeEvent("e2", "dev_a", 20, 2, "project.added", "{}"),
		makeEvent("e3", "dev_a", 30, 3, "project.added", "{}"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.PutRetention(ctx, r2ManifestBytes(t, map[string]int64{"dev_a": 3}), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pull(ctx, dssync.Cursor{"dev_a": 2}); err != nil {
		t.Fatalf("cursor at floor boundary must pull incrementally: %v", err)
	}
	if _, err := h.Pull(ctx, dssync.Cursor{"dev_a": 1}); !errors.Is(err, dssync.ErrSnapshotRequired) {
		t.Fatalf("cursor below floor: got %v, want ErrSnapshotRequired", err)
	}
	if _, err := h.Pull(ctx, nil); !errors.Is(err, dssync.ErrSnapshotRequired) {
		t.Fatalf("fresh cursor below floor: got %v, want ErrSnapshotRequired", err)
	}
}

func TestR2PullFailsClosedOnGarbledManifest(t *testing.T) {
	ctx := context.Background()
	h, m := testR2SnapshotHub()
	if err := m.PutObject(ctx, "workspaces/ws_test/meta/retention.json", []byte("not json"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pull(ctx, nil); err == nil || errors.Is(err, dssync.ErrSnapshotRequired) {
		t.Fatalf("garbled manifest must be a hard error, got %v", err)
	}
}

func TestR2SnapshotObjectRoundTrip(t *testing.T) {
	ctx := context.Background()
	h, _ := testR2SnapshotHub()
	wck, err := dssync.NewWCK()
	if err != nil {
		t.Fatal(err)
	}
	obj, sha, err := dssync.SealSnapshot(dssync.Snapshot{WorkspaceID: "ws_test", ProducedBy: "dev_a", HLC: 1}, wck, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.PutSnapshotObject(ctx, sha, obj); err != nil {
		t.Fatal(err)
	}
	if err := h.PutSnapshotObject(ctx, sha, obj); err != nil {
		t.Fatalf("content-addressed re-put must dedup: %v", err)
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
	if len(list) != 1 || list[0].Key != sha || list[0].LastModified.IsZero() {
		t.Fatalf("list: %+v", list)
	}
	if err := h.DeleteSnapshotObject(ctx, sha); err != nil {
		t.Fatal(err)
	}
	if _, err := h.GetSnapshotObject(ctx, sha); !errors.Is(err, dssync.ErrBlobNotFound) {
		t.Fatalf("got %v, want ErrBlobNotFound", err)
	}
}

// TestR2CompactEventsBelowBothLayouts seeds the seq-keyed layout via Push and
// the retired HLC-keyed layout directly, then proves compaction deletes only
// strictly-below-floor objects in both, keeps unparseable legacy keys, and
// leaves other devices untouched.
func TestR2CompactEventsBelowBothLayouts(t *testing.T) {
	ctx := context.Background()
	h, m := testR2SnapshotHub()
	if err := h.Push(ctx, []state.Event{
		makeEvent("a1", "dev_a", 10, 1, "project.added", "{}"),
		makeEvent("a2", "dev_a", 20, 2, "project.added", "{}"),
		makeEvent("a3", "dev_a", 30, 3, "project.added", "{}"),
		makeEvent("b1", "dev_b", 15, 1, "project.added", "{}"),
	}); err != nil {
		t.Fatal(err)
	}
	// Legacy objects: one below dev_a's floor, one above, one unparseable.
	legacyBelow := "workspaces/ws_test/events/00000000000000000005/dev_a/1/l1.json"
	legacyAbove := "workspaces/ws_test/events/00000000000000000040/dev_a/9/l9.json"
	legacyJunk := "workspaces/ws_test/events/junkkey"
	for _, k := range []string{legacyBelow, legacyAbove, legacyJunk} {
		if err := m.PutObject(ctx, k, []byte("{}"), false); err != nil {
			t.Fatal(err)
		}
	}
	deleted, err := h.CompactEventsBelow(ctx, dssync.Cursor{"dev_a": 3})
	if err != nil {
		t.Fatal(err)
	}
	// a1, a2 (seq layout) + the legacy seq-1 object.
	if deleted != 3 {
		t.Fatalf("deleted = %d, want 3", deleted)
	}
	mustExist := []string{
		"workspaces/ws_test/eventlog/dev_a/" + fmt.Sprintf("%020d_a3.json", 3),
		"workspaces/ws_test/eventlog/dev_b/" + fmt.Sprintf("%020d_b1.json", 1),
		legacyAbove,
		legacyJunk,
	}
	for _, k := range mustExist {
		ok, err := m.ObjectExists(ctx, k)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Errorf("object %s must survive compaction", k)
		}
	}
	mustBeGone := []string{
		"workspaces/ws_test/eventlog/dev_a/" + fmt.Sprintf("%020d_a1.json", 1),
		"workspaces/ws_test/eventlog/dev_a/" + fmt.Sprintf("%020d_a2.json", 2),
		legacyBelow,
	}
	for _, k := range mustBeGone {
		ok, err := m.ObjectExists(ctx, k)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Errorf("object %s must be deleted by compaction", k)
		}
	}
}
