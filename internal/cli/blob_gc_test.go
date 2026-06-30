package cli

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
)

// faultHub wraps a Hub, counting PutBlob/DeleteBlob and optionally failing
// PutBlob, to verify the SEC-01 rewrap hub-cleanup gating.
type faultHub struct {
	dssync.Hub
	putErr  error
	Puts    int
	Deletes int
}

func (f *faultHub) PutBlob(ctx context.Context, hash string, r io.Reader) error {
	f.Puts++
	if f.putErr != nil {
		return f.putErr
	}
	return f.Hub.PutBlob(ctx, hash, r)
}

func (f *faultHub) DeleteBlob(ctx context.Context, hash string) error {
	f.Deletes++
	return f.Hub.DeleteBlob(ctx, hash)
}

func newRewrapTestStore(t *testing.T) *state.Store {
	t.Helper()
	st, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(context.Background(), "test", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureDevice(context.Background(), "device-a"); err != nil {
		t.Fatal(err)
	}
	return st
}

// TestRewrapHubCleanupKeepsOldBlobOnPushFailure (SEC-01 regression): when the
// rewrapped blob fails to upload, the old ciphertext must NOT be deleted from
// the hub, otherwise the hub loses both copies (data loss).
func TestRewrapHubCleanupKeepsOldBlobOnPushFailure(t *testing.T) {
	ctx := context.Background()
	st := newRewrapTestStore(t)
	hub := &faultHub{Hub: dssync.FileHub{Path: filepath.Join(t.TempDir(), "hub.json")}, putErr: errors.New("upload failed")}

	rewrapHubCleanup(ctx, hub, st, "age_blob:"+hex64a, "age_blob:"+hex64b, []byte("rewrapped"))

	if hub.Puts != 1 {
		t.Fatalf("PutBlob calls = %d, want 1", hub.Puts)
	}
	if hub.Deletes != 0 {
		t.Fatalf("DeleteBlob calls = %d, want 0 (old ciphertext kept on push failure)", hub.Deletes)
	}
}

// TestRewrapHubCleanupDeletesOldBlobOnSuccess: on a successful push, when no
// binding references the old ref, the old ciphertext is deleted.
func TestRewrapHubCleanupDeletesOldBlobOnSuccess(t *testing.T) {
	ctx := context.Background()
	st := newRewrapTestStore(t)
	hub := &faultHub{Hub: dssync.FileHub{Path: filepath.Join(t.TempDir(), "hub.json")}}

	rewrapHubCleanup(ctx, hub, st, "age_blob:"+hex64a, "age_blob:"+hex64b, []byte("rewrapped"))

	if hub.Puts != 1 {
		t.Fatalf("PutBlob calls = %d, want 1", hub.Puts)
	}
	if hub.Deletes != 1 {
		t.Fatalf("DeleteBlob calls = %d, want 1 (old ciphertext deleted on success)", hub.Deletes)
	}
}

const hex64a = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const hex64b = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
