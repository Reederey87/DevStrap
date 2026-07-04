package hub

import (
	"context"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
)

// TestR2AckPlaneRoundTrip mirrors the FileHub ack-plane conformance contract
// against the R2 backend over memS3: PutAck overwrite, ListAcks, idempotent
// DeleteAck (P4-SYNC-06).
func TestR2AckPlaneRoundTrip(t *testing.T) {
	ctx := context.Background()
	h, _ := testR2SnapshotHub()

	if acks, err := h.ListAcks(ctx); err != nil || len(acks) != 0 {
		t.Fatalf("empty hub ListAcks = %v, %v; want empty", acks, err)
	}
	if err := h.PutAck(ctx, "dev_a", []byte(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := h.PutAck(ctx, "dev_b", []byte(`{"v":1,"device_id":"dev_b"}`)); err != nil {
		t.Fatal(err)
	}
	// Last-writer-wins overwrite (unconditional PUT).
	if err := h.PutAck(ctx, "dev_a", []byte(`{"v":1,"hlc_watermark":42}`)); err != nil {
		t.Fatal(err)
	}
	acks, err := h.ListAcks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(acks) != 2 {
		t.Fatalf("ListAcks returned %d, want 2: %v", len(acks), acks)
	}
	if string(acks["dev_a"]) != `{"v":1,"hlc_watermark":42}` {
		t.Errorf("dev_a ack not overwritten: %s", acks["dev_a"])
	}
	if err := h.DeleteAck(ctx, "dev_a"); err != nil {
		t.Fatal(err)
	}
	if err := h.DeleteAck(ctx, "dev_a"); err != nil {
		t.Fatalf("second DeleteAck must be idempotent: %v", err)
	}
	acks, err = h.ListAcks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := acks["dev_a"]; ok {
		t.Error("dev_a ack still present after delete")
	}
	if _, ok := acks["dev_b"]; !ok {
		t.Error("dev_b ack unexpectedly removed")
	}
}

func TestR2AckRejectsUnsafeDeviceID(t *testing.T) {
	ctx := context.Background()
	h, _ := testR2SnapshotHub()
	if err := h.PutAck(ctx, "../evil", []byte("x")); err == nil {
		t.Fatal("PutAck accepted a device id with a path separator")
	}
	if err := h.DeleteAck(ctx, "a/b"); err == nil {
		t.Fatal("DeleteAck accepted a device id with a path separator")
	}
	if _, err := h.DeleteDeviceStream(ctx, ".."); err == nil {
		t.Fatal("DeleteDeviceStream accepted an unsafe device id")
	}
}

// TestR2DeleteDeviceStream removes exactly one device's event objects.
func TestR2DeleteDeviceStream(t *testing.T) {
	ctx := context.Background()
	h, _ := testR2SnapshotHub()
	events := []state.Event{
		makeEvent("e1", "dev_a", 1, 1, "project.added", "{}"),
		makeEvent("e2", "dev_a", 2, 2, "project.added", "{}"),
		makeEvent("e3", "dev_b", 3, 1, "project.added", "{}"),
	}
	if err := h.Push(ctx, events); err != nil {
		t.Fatal(err)
	}
	deleted, err := h.DeleteDeviceStream(ctx, "dev_a")
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Fatalf("DeleteDeviceStream deleted %d, want 2", deleted)
	}
	remaining, err := h.Pull(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].DeviceID != "dev_b" {
		t.Fatalf("after DeleteDeviceStream, remaining = %+v; want only dev_b", remaining)
	}
	if n, err := h.DeleteDeviceStream(ctx, "dev_a"); err != nil || n != 0 {
		t.Fatalf("second DeleteDeviceStream = %d, %v; want 0, nil", n, err)
	}
}
