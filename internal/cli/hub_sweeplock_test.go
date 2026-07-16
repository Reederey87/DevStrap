package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dssync "github.com/Reederey87/DevStrap/internal/sync"
)

// fakeSweepHub is a Hub double that exposes ONLY the sweep-lock methods with
// controllable state, so hubSweepLock's acquire/refuse/break/release logic can
// be driven deterministically. The embedded nil Hub is never called by the
// helper. migrateErr, when set, makes MigrateLegacyEvents fail so the
// release-on-error path is observable.
type fakeSweepHub struct {
	dssync.Hub
	lock       []byte
	lockAt     time.Time
	migrateErr error
	migrated   bool
}

func (f *fakeSweepHub) PutSweepLock(_ context.Context, raw []byte) error {
	if f.lock != nil {
		return dssync.ErrSweepLockHeld
	}
	f.lock = raw
	f.lockAt = time.Now()
	return nil
}

func (f *fakeSweepHub) GetSweepLock(_ context.Context) ([]byte, time.Time, error) {
	if f.lock == nil {
		return nil, time.Time{}, dssync.ErrSweepLockNotFound
	}
	return f.lock, f.lockAt, nil
}

func (f *fakeSweepHub) DeleteSweepLock(_ context.Context) error {
	f.lock = nil
	f.lockAt = time.Time{}
	return nil
}

func (f *fakeSweepHub) MigrateLegacyEvents(_ context.Context, _ bool) (int, int, error) {
	f.migrated = true
	if f.migrateErr != nil {
		return 0, 0, f.migrateErr
	}
	return 2, 1, nil
}

func TestHubSweepLockAcquireAndRelease(t *testing.T) {
	ctx := context.Background()
	st := newRewrapTestStore(t)
	f := &fakeSweepHub{}

	release, err := hubSweepLock(ctx, st, f, time.Hour)
	if err != nil {
		t.Fatalf("hubSweepLock: %v", err)
	}
	if f.lock == nil {
		t.Fatal("lock not acquired")
	}
	dev, _ := st.CurrentDevice(ctx)
	if !strings.Contains(string(f.lock), dev.ID) {
		t.Fatalf("lock body %q does not name the local device %s", f.lock, dev.ID)
	}
	release()
	if f.lock != nil {
		t.Fatal("release() did not delete the lock")
	}
}

func TestHubSweepLockRefusesWhenHeldFresh(t *testing.T) {
	ctx := context.Background()
	st := newRewrapTestStore(t)
	held, _ := dssync.MarshalSweepLock(dssync.SweepLock{HolderDevice: "dev_other", AcquiredAtHLC: 1, TTLSeconds: 3600})
	f := &fakeSweepHub{lock: held, lockAt: time.Now()}

	_, err := hubSweepLock(ctx, st, f, time.Hour)
	if err == nil {
		t.Fatal("hubSweepLock acquired a fresh held lock, want refusal")
	}
	var app appError
	if !errors.As(err, &app) || app.code != exitConflict {
		t.Fatalf("err = %v, want appError exitConflict", err)
	}
	if !strings.Contains(err.Error(), "dev_other") {
		t.Fatalf("refusal %q does not name the holder", err.Error())
	}
	if string(f.lock) != string(held) {
		t.Fatal("a fresh held lock must not be broken")
	}
}

func TestHubSweepLockBreaksStale(t *testing.T) {
	ctx := context.Background()
	st := newRewrapTestStore(t)
	// A 1s-TTL lock last modified two hours ago is stale.
	held, _ := dssync.MarshalSweepLock(dssync.SweepLock{HolderDevice: "dev_other", AcquiredAtHLC: 1, TTLSeconds: 1})
	f := &fakeSweepHub{lock: held, lockAt: time.Now().Add(-2 * time.Hour)}

	release, err := hubSweepLock(ctx, st, f, time.Hour)
	if err != nil {
		t.Fatalf("hubSweepLock did not break a stale lock: %v", err)
	}
	dev, _ := st.CurrentDevice(ctx)
	if !strings.Contains(string(f.lock), dev.ID) {
		t.Fatalf("after breaking the stale lock, holder = %q, want the local device %s", f.lock, dev.ID)
	}
	release()
	if f.lock != nil {
		t.Fatal("release() did not delete the lock")
	}
}

func TestHubMigrateEventsAcquiresLockAndReleases(t *testing.T) {
	ctx := context.Background()
	st := newRewrapTestStore(t)
	f := &fakeSweepHub{}

	migrated, kept, err := hubMigrateEvents(ctx, st, f, false)
	if err != nil {
		t.Fatalf("hubMigrateEvents: %v", err)
	}
	if migrated != 2 || kept != 1 {
		t.Fatalf("hubMigrateEvents = (%d, %d), want (2, 1)", migrated, kept)
	}
	if !f.migrated {
		t.Fatal("MigrateLegacyEvents was not called")
	}
	if f.lock != nil {
		t.Fatal("lock not released after migrate-events")
	}
}

func TestHubMigrateEventsDryRunTakesNoLock(t *testing.T) {
	ctx := context.Background()
	st := newRewrapTestStore(t)
	// A fresh competing lock is present; a dry run must not touch or contend it.
	held, _ := dssync.MarshalSweepLock(dssync.SweepLock{HolderDevice: "dev_other", AcquiredAtHLC: 1, TTLSeconds: 3600})
	f := &fakeSweepHub{lock: held, lockAt: time.Now()}

	if _, _, err := hubMigrateEvents(ctx, st, f, true); err != nil {
		t.Fatalf("dry-run hubMigrateEvents contended the lock: %v", err)
	}
	if string(f.lock) != string(held) {
		t.Fatal("dry run disturbed the existing lock")
	}
}

func TestHubMigrateEventsReleasesLockOnError(t *testing.T) {
	ctx := context.Background()
	st := newRewrapTestStore(t)
	f := &fakeSweepHub{migrateErr: errors.New("boom")}

	_, _, err := hubMigrateEvents(ctx, st, f, false)
	if err == nil {
		t.Fatal("hubMigrateEvents swallowed the migration error")
	}
	if f.lock != nil {
		t.Fatal("lock leaked after a mid-sweep error")
	}
}

// countingLockHub delegates every Hub method to an embedded backend but counts
// the sweep-lock calls, so gc/compact can be asserted to acquire and release.
type countingLockHub struct {
	dssync.Hub
	puts, deletes int
}

func (c *countingLockHub) PutSweepLock(ctx context.Context, raw []byte) error {
	c.puts++
	return c.Hub.PutSweepLock(ctx, raw)
}

func (c *countingLockHub) DeleteSweepLock(ctx context.Context) error {
	c.deletes++
	return c.Hub.DeleteSweepLock(ctx)
}

func TestHubGCAcquiresAndReleasesSweepLock(t *testing.T) {
	ctx := context.Background()
	st := newRewrapTestStore(t)
	hub := &countingLockHub{Hub: dssync.FileHub{Path: filepath.Join(t.TempDir(), "hub.json")}}

	if _, _, _, err := hubGC(ctx, io.Discard, st, hub, "test-hub", testGCPaths(t), 1, 0, false); err != nil {
		t.Fatalf("hubGC: %v", err)
	}
	if hub.puts != 1 || hub.deletes != 1 {
		t.Fatalf("sweep-lock puts=%d deletes=%d, want 1/1 (acquire + release)", hub.puts, hub.deletes)
	}
}

func TestHubGCDryRunTakesNoSweepLock(t *testing.T) {
	ctx := context.Background()
	st := newRewrapTestStore(t)
	hub := &countingLockHub{Hub: dssync.FileHub{Path: filepath.Join(t.TempDir(), "hub.json")}}

	if _, _, _, err := hubGC(ctx, io.Discard, st, hub, "test-hub", testGCPaths(t), 1, 0, true); err != nil {
		t.Fatalf("hubGC dry-run: %v", err)
	}
	if hub.puts != 0 {
		t.Fatalf("dry-run gc acquired the sweep lock (%d puts)", hub.puts)
	}
}

// TestHubGCGraceWindowProtectsRepushedBlob is the P4-HUB-12 gc-race regression:
// a blob whose original write is older than the grace window but which a late
// device just re-pushed must NOT be deleted, because the dedup re-put refreshed
// its LastModified.
func TestHubGCGraceWindowProtectsRepushedBlob(t *testing.T) {
	ctx := context.Background()
	st := newRewrapTestStore(t)
	hubPath := filepath.Join(t.TempDir(), "hub.json")
	hub := dssync.FileHub{Path: hubPath}
	if err := hub.PutBlob(ctx, hex64b, strings.NewReader("ciphertext")); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	// Age the original write well past the 1h window.
	blobFile := filepath.Join(filepath.Dir(hubPath), "hub-blobs", hex64b+".blob")
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(blobFile, old, old); err != nil {
		t.Fatalf("backdate blob: %v", err)
	}
	// A late device re-references it: the dedup re-put refreshes LastModified.
	if err := hub.PutBlob(ctx, hex64b, strings.NewReader("ciphertext")); err != nil {
		t.Fatalf("re-PutBlob: %v", err)
	}
	// With a 1h grace window the just-refreshed blob is protected.
	_, removed, _, err := hubGC(ctx, io.Discard, st, hub, "test-hub", testGCPaths(t), 1, time.Hour, false)
	if err != nil {
		t.Fatalf("hubGC: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0 (re-pushed blob refreshed and within grace)", removed)
	}
	if _, err := hub.GetBlob(ctx, hex64b); err != nil {
		t.Fatalf("re-pushed blob deleted despite grace window: %v", err)
	}
}

// staleListHub reports a stale-old LastModified from ListBlobs (as if a
// concurrent sync dedup-re-put the object AFTER the list) while StatBlob still
// delegates to the real backend's fresh mtime — driving the P4-HUB-12
// pre-delete revalidation.
type staleListHub struct {
	dssync.Hub
	staleTime time.Time
}

func (s staleListHub) ListBlobs(ctx context.Context) ([]dssync.BlobInfo, error) {
	blobs, err := s.Hub.ListBlobs(ctx)
	for i := range blobs {
		blobs[i].LastModified = s.staleTime
	}
	return blobs, err
}

// TestHubGCRevalidatesBeforeDeleteKeepsRefreshedBlob is the FIX-1 regression: a
// blob whose LISTED mtime is stale-old but whose STAT mtime is fresh (a
// concurrent sync re-referenced it after gc's ListBlobs snapshot) must survive
// the sweep — the sweep lock cannot close this race because it serializes
// sweepers, not syncing devices.
func TestHubGCRevalidatesBeforeDeleteKeepsRefreshedBlob(t *testing.T) {
	ctx := context.Background()
	st := newRewrapTestStore(t)
	hubPath := filepath.Join(t.TempDir(), "hub.json")
	fh := dssync.FileHub{Path: hubPath}
	if err := fh.PutBlob(ctx, hex64b, strings.NewReader("ciphertext")); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	// The object's real (stat) mtime is now; the list reports it as 48h old.
	hub := staleListHub{Hub: fh, staleTime: time.Now().Add(-48 * time.Hour)}

	_, removed, _, err := hubGC(ctx, io.Discard, st, hub, "test-hub", testGCPaths(t), 1, time.Hour, false)
	if err != nil {
		t.Fatalf("hubGC: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0 (StatBlob shows a fresh mtime within grace)", removed)
	}
	if _, err := fh.GetBlob(ctx, hex64b); err != nil {
		t.Fatalf("revalidated fresh blob was deleted from the stale list: %v", err)
	}
}

// TestHubSweepLockReleaseIsOwnerAware is the FIX-2 regression: a sweeper that
// overran its TTL and had its lock stale-broken by a successor must NOT delete
// the successor's lock when its own (late) release runs.
func TestHubSweepLockReleaseIsOwnerAware(t *testing.T) {
	ctx := context.Background()
	stA := newRewrapTestStore(t)
	stB := newRewrapTestStore(t)
	hubPath := filepath.Join(t.TempDir(), "hub.json")
	hub := dssync.FileHub{Path: hubPath}

	releaseA, err := hubSweepLock(ctx, stA, hub, time.Hour)
	if err != nil {
		t.Fatalf("A acquire: %v", err)
	}
	// Backdate the lock object past its TTL so B judges it stale.
	lockPath := filepath.Join(filepath.Dir(hubPath), "hub-meta", "sweep.lock")
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(lockPath, old, old); err != nil {
		t.Fatalf("backdate lock: %v", err)
	}
	// B stale-breaks A's lock and acquires its own.
	releaseB, err := hubSweepLock(ctx, stB, hub, time.Hour)
	if err != nil {
		t.Fatalf("B acquire (stale-break): %v", err)
	}
	// A's late release must leave B's lock intact.
	releaseA()
	if _, _, gerr := hub.GetSweepLock(ctx); gerr != nil {
		t.Fatalf("A's late release removed B's lock: %v", gerr)
	}
	// B's own release still works.
	releaseB()
	if _, _, gerr := hub.GetSweepLock(ctx); !errors.Is(gerr, dssync.ErrSweepLockNotFound) {
		t.Fatalf("B's release did not remove its own lock: %v", gerr)
	}
}

// TestHubCompactRefusesWhenSweepLockHeld proves compact acquires the sweep lock
// before its destructive publish: a fresh competing lock makes it refuse with
// exitConflict, and nothing is published.
func TestHubCompactRefusesWhenSweepLockHeld(t *testing.T) {
	env, store, _ := setupCompact(t)
	defer closeStore(store)

	// A cooperating peer holds a fresh sweep lock on the backend.
	fh := dssync.FileHub{Path: env.hubPath}
	held, _ := dssync.MarshalSweepLock(dssync.SweepLock{HolderDevice: "dev_peer", AcquiredAtHLC: 1, TTLSeconds: 3600})
	if err := fh.PutSweepLock(env.ctx, held); err != nil {
		t.Fatalf("seed competing lock: %v", err)
	}

	var out bytes.Buffer
	err := hubCompact(env.ctx, &out, io.Discard, env.opts, store, env.hub(t, store), env.hubID, env.paths, 2, 0, true, false)
	var app appError
	if !errors.As(err, &app) || app.code != exitConflict {
		t.Fatalf("hubCompact err = %v, want appError exitConflict", err)
	}
	if !strings.Contains(err.Error(), "dev_peer") {
		t.Fatalf("refusal %q does not name the holder", err.Error())
	}
	if _, _, gerr := fh.GetRetention(env.ctx); !errors.Is(gerr, dssync.ErrRetentionNotFound) {
		t.Fatalf("compact published a manifest despite the held lock: %v", gerr)
	}
}
