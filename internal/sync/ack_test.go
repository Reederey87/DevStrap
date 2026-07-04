package sync

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
)

func TestAckMarkerSignVerifyRoundTrip(t *testing.T) {
	private, public := testSigningKeys(t)
	m := AckMarker{
		Cursor:           map[string]int64{"dev_a": 5, "dev_b": 3},
		DeviceID:         "dev_self",
		HLCWatermark:     4200,
		ProducedAt:       4200,
		PushedThroughSeq: 12,
		WorkspaceID:      "ws_test",
	}
	if err := SignAckMarker(&m, private); err != nil {
		t.Fatal(err)
	}
	if m.V != ackVersion || m.Sig == "" {
		t.Fatalf("sign did not stamp v/sig: %+v", m)
	}
	if err := VerifyAckMarker(m, public); err != nil {
		t.Fatal(err)
	}
	// JSON round-trip (what the hub stores) must still verify.
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseAckMarker(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAckMarker(parsed, public); err != nil {
		t.Fatalf("round-tripped ack failed verification: %v", err)
	}
}

func TestAckMarkerNilCursorSignsAndVerifies(t *testing.T) {
	private, public := testSigningKeys(t)
	m := AckMarker{DeviceID: "dev_self", WorkspaceID: "ws_test", HLCWatermark: 1}
	if err := SignAckMarker(&m, private); err != nil {
		t.Fatal(err)
	}
	// A device with no consumed peer stream (nil cursor) must verify over the
	// same canonical bytes as an explicit empty map.
	if err := VerifyAckMarker(m, public); err != nil {
		t.Fatalf("nil-cursor ack failed verification: %v", err)
	}
	m.Cursor = map[string]int64{}
	if err := VerifyAckMarker(m, public); err != nil {
		t.Fatalf("empty-map ack failed verification: %v", err)
	}
}

func TestAckMarkerTamperFailsVerification(t *testing.T) {
	private, public := testSigningKeys(t)
	base := AckMarker{
		Cursor:           map[string]int64{"dev_a": 5},
		DeviceID:         "dev_self",
		HLCWatermark:     4200,
		ProducedAt:       4200,
		PushedThroughSeq: 12,
		WorkspaceID:      "ws_test",
	}
	if err := SignAckMarker(&base, private); err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(m *AckMarker){
		"cursor raised":    func(m *AckMarker) { m.Cursor["dev_a"] = 99 },
		"cursor added":     func(m *AckMarker) { m.Cursor["dev_evil"] = 1 },
		"watermark raised": func(m *AckMarker) { m.HLCWatermark = 999999 },
		"push raised":      func(m *AckMarker) { m.PushedThroughSeq = 999 },
		"device swapped":   func(m *AckMarker) { m.DeviceID = "dev_evil" },
		"workspace swap":   func(m *AckMarker) { m.WorkspaceID = "ws_evil" },
		"sig stripped":     func(m *AckMarker) { m.Sig = "" },
	}
	for name, f := range mutations {
		m := base
		m.Cursor = map[string]int64{}
		for k, v := range base.Cursor {
			m.Cursor[k] = v
		}
		f(&m)
		if err := VerifyAckMarker(m, public); err == nil {
			t.Errorf("mutation %q verified but must not", name)
		}
	}
}

// TestFileHubAckPlaneRoundTrip exercises PutAck overwrite semantics, ListAcks,
// and idempotent DeleteAck against the file-backed hub.
func TestFileHubAckPlaneRoundTrip(t *testing.T) {
	ctx := context.Background()
	h := FileHub{Path: filepath.Join(t.TempDir(), "hub.json")}

	if acks, err := h.ListAcks(ctx); err != nil || len(acks) != 0 {
		t.Fatalf("empty hub ListAcks = %v, %v; want empty", acks, err)
	}
	if err := h.PutAck(ctx, "dev_a", []byte(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := h.PutAck(ctx, "dev_b", []byte(`{"v":1,"device_id":"dev_b"}`)); err != nil {
		t.Fatal(err)
	}
	// Overwrite (last-writer-wins): dev_a's newer bytes replace the old.
	if err := h.PutAck(ctx, "dev_a", []byte(`{"v":1,"hlc_watermark":42}`)); err != nil {
		t.Fatal(err)
	}
	acks, err := h.ListAcks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(acks) != 2 {
		t.Fatalf("ListAcks returned %d acks, want 2: %v", len(acks), acks)
	}
	if string(acks["dev_a"]) != `{"v":1,"hlc_watermark":42}` {
		t.Errorf("dev_a ack not overwritten: %s", acks["dev_a"])
	}
	// Idempotent delete.
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

// TestFileHubAckRejectsUnsafeDeviceID: a device id with a path separator cannot
// escape the acks prefix.
func TestFileHubAckRejectsUnsafeDeviceID(t *testing.T) {
	ctx := context.Background()
	h := FileHub{Path: filepath.Join(t.TempDir(), "hub.json")}
	if err := h.PutAck(ctx, "../evil", []byte("x")); err == nil {
		t.Fatal("PutAck accepted a device id with a path separator")
	}
	if err := h.DeleteAck(ctx, "a/b"); err == nil {
		t.Fatal("DeleteAck accepted a device id with a path separator")
	}
}

// TestFileHubDeleteDeviceStream removes exactly one device's events.
func TestFileHubDeleteDeviceStream(t *testing.T) {
	ctx := context.Background()
	h := FileHub{Path: filepath.Join(t.TempDir(), "hub.json")}
	events := []state.Event{
		{ID: "e1", DeviceID: "dev_a", HLC: 1, Seq: 1},
		{ID: "e2", DeviceID: "dev_a", HLC: 2, Seq: 2},
		{ID: "e3", DeviceID: "dev_b", HLC: 3, Seq: 1},
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
	remaining, err := h.Pull(ctx, Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].DeviceID != "dev_b" {
		t.Fatalf("after DeleteDeviceStream, remaining = %+v; want only dev_b", remaining)
	}
	// Idempotent: deleting again removes nothing.
	if n, err := h.DeleteDeviceStream(ctx, "dev_a"); err != nil || n != 0 {
		t.Fatalf("second DeleteDeviceStream = %d, %v; want 0, nil", n, err)
	}
}
