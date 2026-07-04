package sync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSweepLockMarshalRoundTrip(t *testing.T) {
	// AcquiredAtHLC packs a physical-millis timestamp in its high bits.
	physicalMs := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC).UnixMilli()
	l := SweepLock{HolderDevice: "dev_a", AcquiredAtHLC: pack(physicalMs, 0), TTLSeconds: 3600}
	raw, err := MarshalSweepLock(l)
	if err != nil {
		t.Fatalf("MarshalSweepLock: %v", err)
	}
	got, err := ParseSweepLock(raw)
	if err != nil {
		t.Fatalf("ParseSweepLock: %v", err)
	}
	if got != l {
		t.Fatalf("round-trip = %+v, want %+v", got, l)
	}
	if got.TTL() != time.Hour {
		t.Fatalf("TTL = %v, want 1h", got.TTL())
	}
	if !got.AcquiredAt().Equal(time.UnixMilli(physicalMs)) {
		t.Fatalf("AcquiredAt = %v, want %v", got.AcquiredAt(), time.UnixMilli(physicalMs))
	}
}

// TestFileHubSweepLockLifecycle: create-only PutSweepLock, second PUT is
// ErrSweepLockHeld, GetSweepLock returns the body and a real file mtime, and
// DeleteSweepLock releases it (idempotently).
func TestFileHubSweepLockLifecycle(t *testing.T) {
	ctx := context.Background()
	h := FileHub{Path: filepath.Join(t.TempDir(), "hub.json")}

	if _, _, err := h.GetSweepLock(ctx); !errors.Is(err, ErrSweepLockNotFound) {
		t.Fatalf("GetSweepLock on empty = %v, want ErrSweepLockNotFound", err)
	}
	body := []byte(`{"holder_device":"dev_a","acquired_at_hlc":1,"ttl_seconds":3600}`)
	if err := h.PutSweepLock(ctx, body); err != nil {
		t.Fatalf("PutSweepLock: %v", err)
	}
	if err := h.PutSweepLock(ctx, body); !errors.Is(err, ErrSweepLockHeld) {
		t.Fatalf("second PutSweepLock = %v, want ErrSweepLockHeld", err)
	}
	raw, mtime, err := h.GetSweepLock(ctx)
	if err != nil {
		t.Fatalf("GetSweepLock: %v", err)
	}
	if string(raw) != string(body) {
		t.Fatalf("GetSweepLock body = %q, want %q", raw, body)
	}
	if mtime.IsZero() {
		t.Fatalf("GetSweepLock returned a zero mtime")
	}
	if err := h.DeleteSweepLock(ctx); err != nil {
		t.Fatalf("DeleteSweepLock: %v", err)
	}
	if _, _, err := h.GetSweepLock(ctx); !errors.Is(err, ErrSweepLockNotFound) {
		t.Fatalf("GetSweepLock after delete = %v, want ErrSweepLockNotFound", err)
	}
	if err := h.DeleteSweepLock(ctx); err != nil {
		t.Fatalf("idempotent DeleteSweepLock: %v", err)
	}
}

// TestFileHubPutBlobDedupRefreshesMTime: re-putting a content-addressed blob is
// a dedup hit that still bumps the file mtime (P4-HUB-12).
func TestFileHubPutBlobDedupRefreshesMTime(t *testing.T) {
	ctx := context.Background()
	h := FileHub{Path: filepath.Join(t.TempDir(), "hub.json")}
	hash := strings.Repeat("a", 64)

	if err := h.PutBlob(ctx, hash, strings.NewReader("ciphertext")); err != nil {
		t.Fatalf("PutBlob (first): %v", err)
	}
	// Backdate the blob so the refresh is observable regardless of clock
	// resolution.
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(h.blobPath(hash), old, old); err != nil {
		t.Fatalf("backdate blob: %v", err)
	}
	if err := h.PutBlob(ctx, hash, strings.NewReader("ciphertext")); err != nil {
		t.Fatalf("PutBlob (dedup): %v", err)
	}
	info, err := os.Stat(h.blobPath(hash))
	if err != nil {
		t.Fatalf("stat blob: %v", err)
	}
	if !info.ModTime().After(old.Add(time.Hour)) {
		t.Fatalf("dedup PutBlob did not refresh mtime: got %v, backdated %v", info.ModTime(), old)
	}
}

// TestFileHubMigrateLegacyEventsNoOp: FileHub never used the legacy layout.
func TestFileHubMigrateLegacyEventsNoOp(t *testing.T) {
	ctx := context.Background()
	h := FileHub{Path: filepath.Join(t.TempDir(), "hub.json")}
	for _, dry := range []bool{false, true} {
		m, k, err := h.MigrateLegacyEvents(ctx, dry)
		if err != nil || m != 0 || k != 0 {
			t.Fatalf("MigrateLegacyEvents(dry=%v) = (%d, %d, %v), want (0, 0, nil)", dry, m, k, err)
		}
	}
}
