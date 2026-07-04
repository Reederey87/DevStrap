package hub

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	dssync "github.com/Reederey87/DevStrap/internal/sync"
)

// TestR2SweepLockLifecycle: create-only PUT, a second PUT is ErrSweepLockHeld,
// GET returns the body with a non-zero backend mtime, and DELETE releases it.
func TestR2SweepLockLifecycle(t *testing.T) {
	ctx := context.Background()
	h := newTestR2Hub(t)

	if _, _, err := h.GetSweepLock(ctx); !errors.Is(err, dssync.ErrSweepLockNotFound) {
		t.Fatalf("GetSweepLock on empty = %v, want ErrSweepLockNotFound", err)
	}

	body := []byte(`{"holder_device":"dev_a","acquired_at_hlc":1,"ttl_seconds":3600}`)
	if err := h.PutSweepLock(ctx, body); err != nil {
		t.Fatalf("PutSweepLock: %v", err)
	}
	if err := h.PutSweepLock(ctx, body); !errors.Is(err, dssync.ErrSweepLockHeld) {
		t.Fatalf("second PutSweepLock = %v, want ErrSweepLockHeld", err)
	}

	raw, lastModified, err := h.GetSweepLock(ctx)
	if err != nil {
		t.Fatalf("GetSweepLock: %v", err)
	}
	if string(raw) != string(body) {
		t.Fatalf("GetSweepLock body = %q, want %q", raw, body)
	}
	if lastModified.IsZero() {
		t.Fatalf("GetSweepLock returned a zero LastModified; the stale-break judgment needs a backend mtime")
	}

	if err := h.DeleteSweepLock(ctx); err != nil {
		t.Fatalf("DeleteSweepLock: %v", err)
	}
	if _, _, err := h.GetSweepLock(ctx); !errors.Is(err, dssync.ErrSweepLockNotFound) {
		t.Fatalf("GetSweepLock after delete = %v, want ErrSweepLockNotFound", err)
	}
	// DeleteSweepLock is idempotent.
	if err := h.DeleteSweepLock(ctx); err != nil {
		t.Fatalf("idempotent DeleteSweepLock: %v", err)
	}
}

// TestR2PutBlobDedupRefreshesLastModified: re-putting a content-addressed blob
// is a dedup hit but refreshes the object's LastModified so `hub gc`'s grace
// window protects a just-re-referenced blob (P4-HUB-12 / P6-HUB-01).
func TestR2PutBlobDedupRefreshesLastModified(t *testing.T) {
	ctx := context.Background()
	h := newTestR2Hub(t)
	hash := strings.Repeat("a", 64)

	if err := h.PutBlob(ctx, hash, strings.NewReader("ciphertext")); err != nil {
		t.Fatalf("PutBlob (first): %v", err)
	}
	first := blobModTime(t, ctx, h, hash)

	// Re-put identical bytes: a dedup hit that must still bump LastModified.
	if err := h.PutBlob(ctx, hash, strings.NewReader("ciphertext")); err != nil {
		t.Fatalf("PutBlob (dedup): %v", err)
	}
	second := blobModTime(t, ctx, h, hash)
	if !second.After(first) {
		t.Fatalf("dedup PutBlob did not refresh LastModified: first=%v second=%v", first, second)
	}
}

func blobModTime(t *testing.T, ctx context.Context, h R2Hub, hash string) time.Time {
	t.Helper()
	blobs, err := h.ListBlobs(ctx)
	if err != nil {
		t.Fatalf("ListBlobs: %v", err)
	}
	for _, b := range blobs {
		if b.Key == hash {
			return b.LastModified
		}
	}
	t.Fatalf("blob %s not found in ListBlobs", hash)
	return time.Time{}
}
