package hub

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
)

func newTestR2Hub(t *testing.T) R2Hub {
	t.Helper()
	return R2Hub{S3: newMemS3(), WorkspaceID: "ws_test"}
}

func TestR2EventKeyImmutable(t *testing.T) {
	h := newTestR2Hub(t)
	e := makeEvent("evt_001", "dev_a", 123456, 1, "project.added", `{"path":"x"}`)
	key := h.eventKey(e)
	want := "workspaces/ws_test/events/00000000000000123456/dev_a/1/evt_001.json"
	if key != want {
		t.Errorf("eventKey = %q, want %q", key, want)
	}
}

func TestR2PushPullRoundTrip(t *testing.T) {
	ctx := context.Background()
	h := newTestR2Hub(t)
	events := []state.Event{
		makeEvent("evt_001", "dev_a", 100, 1, "project.added", `{"path":"a"}`),
		makeEvent("evt_002", "dev_b", 200, 1, "project.added", `{"path":"b"}`),
		makeEvent("evt_003", "dev_a", 150, 2, "project.updated", `{"path":"a"}`),
	}
	if err := h.Push(ctx, events); err != nil {
		t.Fatalf("Push: %v", err)
	}
	pulled, err := h.Pull(ctx, 0)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(pulled) != 3 {
		t.Fatalf("Pull returned %d events, want 3", len(pulled))
	}
	// Deterministic order: HLC, device_id, id.
	if pulled[0].HLC != 100 || pulled[1].HLC != 150 || pulled[2].HLC != 200 {
		t.Errorf("Pull order: HLCs = %d %d %d, want 100 150 200", pulled[0].HLC, pulled[1].HLC, pulled[2].HLC)
	}
}

func TestR2PullCursorIncremental(t *testing.T) {
	ctx := context.Background()
	h := newTestR2Hub(t)
	events := []state.Event{
		makeEvent("evt_001", "dev_a", 100, 1, "project.added", `{"path":"a"}`),
		makeEvent("evt_002", "dev_a", 200, 2, "project.added", `{"path":"b"}`),
	}
	if err := h.Push(ctx, events); err != nil {
		t.Fatalf("Push: %v", err)
	}
	// EAGER-02: cursor-based pull returns only events after the cursor.
	pulled, err := h.Pull(ctx, 100)
	if err != nil {
		t.Fatalf("Pull(100): %v", err)
	}
	if len(pulled) != 1 || pulled[0].ID != "evt_002" {
		t.Errorf("Pull(100) = %v, want only evt_002", pulled)
	}
	// Pull past everything returns nothing.
	pulled, err = h.Pull(ctx, 200)
	if err != nil {
		t.Fatalf("Pull(200): %v", err)
	}
	if len(pulled) != 0 {
		t.Errorf("Pull(200) = %d events, want 0", len(pulled))
	}
}

func TestR2PushIdempotent(t *testing.T) {
	ctx := context.Background()
	h := newTestR2Hub(t)
	e := makeEvent("evt_001", "dev_a", 100, 1, "project.added", `{"path":"a"}`)
	if err := h.Push(ctx, []state.Event{e}); err != nil {
		t.Fatalf("Push 1: %v", err)
	}
	// HUB-06: re-pushing the same event is a no-op (conditional put).
	if err := h.Push(ctx, []state.Event{e}); err != nil {
		t.Fatalf("Push 2: %v", err)
	}
	pulled, _ := h.Pull(ctx, 0)
	if len(pulled) != 1 {
		t.Errorf("after duplicate push, got %d events, want 1", len(pulled))
	}
}

func TestR2BlobPutGet(t *testing.T) {
	ctx := context.Background()
	h := newTestR2Hub(t)
	data := []byte("encrypted-blob-content")
	if err := h.PutBlob(ctx, strings.Repeat("a", 64), bytes.NewReader(data)); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	// Idempotent: same content-addressed key is a no-op.
	if err := h.PutBlob(ctx, strings.Repeat("a", 64), bytes.NewReader(data)); err != nil {
		t.Fatalf("PutBlob 2: %v", err)
	}
	rc, err := h.GetBlob(ctx, strings.Repeat("a", 64))
	if err != nil {
		t.Fatalf("GetBlob: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, data) {
		t.Errorf("GetBlob = %q, want %q", got, data)
	}
}

func TestR2BlobNotFound(t *testing.T) {
	ctx := context.Background()
	h := newTestR2Hub(t)
	_, err := h.GetBlob(ctx, strings.Repeat("b", 64))
	if err == nil {
		t.Fatal("expected error for missing blob")
	}
}

func TestR2InvalidBlobKey(t *testing.T) {
	ctx := context.Background()
	h := newTestR2Hub(t)
	if err := h.PutBlob(ctx, "short", bytes.NewReader([]byte("x"))); err == nil {
		t.Error("expected error for invalid blob key")
	}
}

func TestR2Pagination(t *testing.T) {
	ctx := context.Background()
	h := newTestR2Hub(t)
	// Push enough events to exercise pagination (maxKeys=1000 in R2Hub).
	for i := 0; i < 5; i++ {
		e := makeEvent(
			"evt_"+string(rune('a'+i)),
			"dev_a",
			int64(100+i),
			int64(i+1),
			"project.added",
			`{"path":"x"}`,
		)
		if err := h.Push(ctx, []state.Event{e}); err != nil {
			t.Fatalf("Push %d: %v", i, err)
		}
	}
	// Force small page size by directly using the S3 client.
	prefix := h.eventsPrefix()
	keys, _, err := h.S3.ListObjectsV2(ctx, prefix, "", 3)
	if err != nil {
		t.Fatalf("ListObjectsV2: %v", err)
	}
	if len(keys) != 3 {
		t.Errorf("page size = %d, want 3", len(keys))
	}
}
