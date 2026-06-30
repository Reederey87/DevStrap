package cli

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
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

	rewrapHubCleanup(ctx, hub, st, "age_blob:"+hex64a, "age_blob:"+hex64b, []byte("rewrapped"), nil)

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

	rewrapHubCleanup(ctx, hub, st, "age_blob:"+hex64a, "age_blob:"+hex64b, []byte("rewrapped"), nil)

	if hub.Puts != 1 {
		t.Fatalf("PutBlob calls = %d, want 1", hub.Puts)
	}
	if hub.Deletes != 1 {
		t.Fatalf("DeleteBlob calls = %d, want 1 (old ciphertext deleted on success)", hub.Deletes)
	}
}

const hex64a = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const hex64b = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const hex64c = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

// P5-HUB-02: PruneDraftSnapshots keeps the latest `keep` snapshots per project
// and deletes the rest, so superseded blobs can be reclaimed.
func TestPruneDraftSnapshots(t *testing.T) {
	ctx := context.Background()
	st := newRewrapTestStore(t)
	if _, err := st.UpsertProject(ctx, state.UpsertProjectParams{Path: "work/d", Type: "draft_project"}); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	proj, err := st.ProjectByPath(ctx, "work/d")
	if err != nil {
		t.Fatal(err)
	}
	snaps := []struct {
		ref string
		id  string
		hlc int64
	}{{hex64a, "evt_a", 10}, {hex64b, "evt_b", 11}, {hex64c, "evt_c", 12}}
	for _, s := range snaps {
		if err := st.RecordDraftSnapshot(ctx, proj.ID, "age_blob:"+s.ref, 1, 1, state.Event{ID: s.id, HLC: s.hlc}); err != nil {
			t.Fatalf("RecordDraftSnapshot %s: %v", s.id, err)
		}
	}
	pruned, err := st.PruneDraftSnapshots(ctx, 1)
	if err != nil {
		t.Fatalf("PruneDraftSnapshots: %v", err)
	}
	if pruned != 2 {
		t.Fatalf("pruned = %d, want 2", pruned)
	}
	latest, err := st.LatestDraftSnapshot(ctx, proj.ID)
	if err != nil || latest == nil {
		t.Fatalf("LatestDraftSnapshot: %v", err)
	}
	if latest.BlobRef != "age_blob:"+hex64c {
		t.Fatalf("retained snapshot = %s, want the highest-HLC age_blob:%s", latest.BlobRef, hex64c)
	}
}

// P5-PROD-02: a blob queued by a prior local-only revoke is deleted from the
// hub on the next hub-enabled sync and removed from the queue.
func TestDrainPendingHubDeletes(t *testing.T) {
	ctx := context.Background()
	st := newRewrapTestStore(t)
	hub := dssync.FileHub{Path: filepath.Join(t.TempDir(), "hub.json")}
	if err := hub.PutBlob(ctx, hex64a, strings.NewReader("old-ciphertext")); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	if err := st.QueuePendingHubDelete(ctx, "age_blob:"+hex64a); err != nil {
		t.Fatalf("QueuePendingHubDelete: %v", err)
	}
	deleted, err := drainPendingHubDeletes(ctx, st, hub)
	if err != nil {
		t.Fatalf("drainPendingHubDeletes: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}
	if _, err := hub.GetBlob(ctx, hex64a); err == nil {
		t.Fatal("queued blob should have been deleted from the hub")
	}
	if refs, _ := st.PendingHubDeletes(ctx); len(refs) != 0 {
		t.Fatalf("queue not cleared after drain: %v", refs)
	}
}

// P5-HUB-02: hubGC deletes hub blobs not referenced by any binding/snapshot,
// and leaves referenced ones alone.
func TestHubGCDeletesUnreferencedBlobs(t *testing.T) {
	ctx := context.Background()
	st := newRewrapTestStore(t)
	hub := dssync.FileHub{Path: filepath.Join(t.TempDir(), "hub.json")}

	// Two blobs on the hub; only hex64a is referenced by a draft snapshot.
	for _, k := range []string{hex64a, hex64b} {
		if err := hub.PutBlob(ctx, k, strings.NewReader("ciphertext-"+k)); err != nil {
			t.Fatalf("PutBlob %s: %v", k, err)
		}
	}
	// Create a project + a draft snapshot referencing hex64a.
	if _, err := st.UpsertProject(ctx, state.UpsertProjectParams{Path: "work/draft", Type: "draft_project"}); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	proj, err := st.ProjectByPath(ctx, "work/draft")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordDraftSnapshot(ctx, proj.ID, "age_blob:"+hex64a, 1, 1, state.Event{ID: "evt_d1", HLC: 10}); err != nil {
		t.Fatalf("RecordDraftSnapshot: %v", err)
	}

	pruned, removed, err := hubGC(ctx, io.Discard, st, hub, 1, false)
	if err != nil {
		t.Fatalf("hubGC: %v", err)
	}
	if pruned != 0 {
		t.Fatalf("pruned = %d, want 0 (only one snapshot)", pruned)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1 (only the unreferenced blob)", removed)
	}
	// hex64a (referenced) survives; hex64b (unreferenced) is gone.
	if _, err := hub.GetBlob(ctx, hex64a); err != nil {
		t.Fatalf("referenced blob hex64a was deleted: %v", err)
	}
	if _, err := hub.GetBlob(ctx, hex64b); err == nil {
		t.Fatal("unreferenced blob hex64b should have been deleted")
	}
}
