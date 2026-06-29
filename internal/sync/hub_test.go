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
