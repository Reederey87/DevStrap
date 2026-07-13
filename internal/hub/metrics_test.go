package hub

import (
	"context"
	"strings"
	"testing"

	dssync "github.com/Reederey87/DevStrap/internal/sync"
)

// TestMeteredS3CountsOpsAndBytes verifies the S3-boundary decorator records one
// op per call and the payload bytes on the transfers that carry them (P4-HUB-14).
func TestMeteredS3CountsOpsAndBytes(t *testing.T) {
	ctx := context.Background()
	m := NewMetrics()
	s := newMeteredS3(newMemS3(), m)

	body := []byte("hello-metrics")
	if err := s.PutObject(ctx, "k1", body, false); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetObject(ctx, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("GetObject = %q, want %q", got, body)
	}
	if _, _, err := s.ListObjectsV2(ctx, "", "", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StatObject(ctx, "k1"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteObject(ctx, "k1"); err != nil {
		t.Fatal(err)
	}

	snap := m.Snapshot()
	if snap.Ops["put"] != 1 || snap.Ops["get"] != 1 || snap.Ops["list"] != 1 || snap.Ops["stat"] != 1 || snap.Ops["delete"] != 1 {
		t.Fatalf("ops = %v, want one each of put/get/list/stat/delete", snap.Ops)
	}
	if snap.TotalOps != 5 {
		t.Fatalf("TotalOps = %d, want 5", snap.TotalOps)
	}
	if snap.BytesUp != int64(len(body)) {
		t.Fatalf("BytesUp = %d, want %d", snap.BytesUp, len(body))
	}
	if snap.BytesDown != int64(len(body)) {
		t.Fatalf("BytesDown = %d, want %d", snap.BytesDown, len(body))
	}
	if s := snap.String(); !strings.Contains(s, "5 ops") || !strings.Contains(s, "put 1") {
		t.Fatalf("Snapshot.String() = %q, want a readable op summary", s)
	}
}

// TestR2HubMetricsWiring checks NewR2Hub wires metering so hub operations
// accumulate and HubMetrics reports them, while a bare struct literal reports
// unavailable (P4-HUB-14).
func TestR2HubMetricsWiring(t *testing.T) {
	ctx := context.Background()
	h := NewR2Hub(newMemS3(), "ws_test")
	if _, err := h.ListBlobs(ctx); err != nil {
		t.Fatal(err)
	}
	snap, ok := h.HubMetrics()
	if !ok {
		t.Fatal("HubMetrics() ok = false, want true for NewR2Hub")
	}
	if snap.TotalOps == 0 {
		t.Fatalf("TotalOps = 0, want the ListBlobs list op counted")
	}

	bare := R2Hub{S3: newMemS3(), WorkspaceID: "ws_test"}
	if _, ok := bare.HubMetrics(); ok {
		t.Fatal("bare R2Hub{} HubMetrics() ok = true, want false (no metering wired)")
	}
	_ = dssync.Hub(bare)
}
