package hub

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
)

func newGitCarrierTestHub(t *testing.T, remote string, name string) *GitCarrierHub {
	t.Helper()
	h, err := NewGitCarrierHub(remote, "main", "ws_test", filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatalf("NewGitCarrierHub(%s): %v", name, err)
	}
	h.sleep = func(time.Duration) {}
	return h
}

func gitCarrierManifestBytes(t *testing.T, floors map[string]int64) []byte {
	t.Helper()
	raw, err := json.Marshal(dssync.RetentionManifest{V: 1, WorkspaceID: "ws_test", Floors: floors})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func assertGitCarrierAckPlane(t *testing.T, ctx context.Context, h dssync.Hub) {
	t.Helper()
	if acks, err := h.ListAcks(ctx); err != nil || len(acks) != 0 {
		t.Fatalf("empty hub ListAcks = %v, %v; want empty", acks, err)
	}
	if err := h.PutAck(ctx, "dev_a", []byte(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := h.PutAck(ctx, "dev_b", []byte(`{"v":1,"device_id":"dev_b"}`)); err != nil {
		t.Fatal(err)
	}
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

func assertGitCarrierRetentionAndSnapshot(t *testing.T, ctx context.Context, h dssync.Hub) {
	t.Helper()
	if _, _, err := h.GetRetention(ctx); !errors.Is(err, dssync.ErrRetentionNotFound) {
		t.Fatalf("got %v, want ErrRetentionNotFound", err)
	}
	if err := h.PutRetention(ctx, gitCarrierManifestBytes(t, map[string]int64{"dev_a": 2}), ""); err != nil {
		t.Fatal(err)
	}
	if err := h.PutRetention(ctx, gitCarrierManifestBytes(t, map[string]int64{"dev_a": 3}), ""); !errors.Is(err, dssync.ErrRetentionConflict) {
		t.Fatalf("second create: got %v, want ErrRetentionConflict", err)
	}
	_, etag, err := h.GetRetention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.PutRetention(ctx, gitCarrierManifestBytes(t, map[string]int64{"dev_a": 4}), etag); err != nil {
		t.Fatal(err)
	}
	if err := h.PutRetention(ctx, gitCarrierManifestBytes(t, map[string]int64{"dev_a": 5}), etag); !errors.Is(err, dssync.ErrRetentionConflict) {
		t.Fatalf("stale etag: got %v, want ErrRetentionConflict", err)
	}
	raw, _, err := h.GetRetention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	floors, err := dssync.ParseRetentionFloors(raw)
	if err != nil {
		t.Fatal(err)
	}
	if floors["dev_a"] != 4 {
		t.Fatalf("floors = %v, want dev_a:4", floors)
	}
	if err := h.Push(ctx, []state.Event{
		makeEvent("floor_1", "dev_floor", 10, 1, "project.added", "{}"),
		makeEvent("floor_2", "dev_floor", 20, 2, "project.added", "{}"),
		makeEvent("floor_3", "dev_floor", 30, 3, "project.added", "{}"),
	}); err != nil {
		t.Fatal(err)
	}
	_, etag, err = h.GetRetention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.PutRetention(ctx, gitCarrierManifestBytes(t, map[string]int64{"dev_floor": 3}), etag); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pull(ctx, dssync.Cursor{"dev_floor": 2}); err != nil {
		t.Fatalf("cursor at floor boundary must pull incrementally: %v", err)
	}
	if _, err := h.Pull(ctx, dssync.Cursor{"dev_floor": 1}); !errors.Is(err, dssync.ErrSnapshotRequired) {
		t.Fatalf("cursor below floor: got %v, want ErrSnapshotRequired", err)
	}

	wck, err := dssync.NewWCK()
	if err != nil {
		t.Fatal(err)
	}
	obj, sha, err := dssync.SealSnapshot(dssync.Snapshot{WorkspaceID: "ws_test", ProducedBy: "dev_a", HLC: 1}, wck, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.PutSnapshotObject(ctx, sha, obj); err != nil {
		t.Fatal(err)
	}
	if err := h.PutSnapshotObject(ctx, sha, obj); err != nil {
		t.Fatalf("content-addressed re-put must dedup: %v", err)
	}
	got, err := h.GetSnapshotObject(ctx, sha)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(obj) {
		t.Fatal("snapshot object bytes did not round-trip")
	}
	list, err := h.ListSnapshotObjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Key != sha || list[0].LastModified.IsZero() {
		t.Fatalf("list: %+v", list)
	}
	if err := h.DeleteSnapshotObject(ctx, sha); err != nil {
		t.Fatal(err)
	}
	if _, err := h.GetSnapshotObject(ctx, sha); !errors.Is(err, dssync.ErrBlobNotFound) {
		t.Fatalf("got %v, want ErrBlobNotFound", err)
	}
}

func assertGitCarrierSweepLock(t *testing.T, ctx context.Context, h dssync.Hub) {
	t.Helper()
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
		t.Fatal("GetSweepLock returned a zero LastModified; the stale-break judgment needs a backend mtime")
	}
	if err := h.DeleteSweepLock(ctx); err != nil {
		t.Fatalf("DeleteSweepLock: %v", err)
	}
	if _, _, err := h.GetSweepLock(ctx); !errors.Is(err, dssync.ErrSweepLockNotFound) {
		t.Fatalf("GetSweepLock after delete = %v, want ErrSweepLockNotFound", err)
	}
	if err := h.DeleteSweepLock(ctx); err != nil {
		t.Fatalf("idempotent DeleteSweepLock: %v", err)
	}
}

func TestGitCarrierConformance(t *testing.T) {
	ctx := context.Background()
	assertHubRoundTrip(t, ctx, newGitCarrierTestHub(t, newBareCarrier(t), "roundtrip"))
	assertGitCarrierAckPlane(t, ctx, newGitCarrierTestHub(t, newBareCarrier(t), "ack"))
	assertGitCarrierRetentionAndSnapshot(t, ctx, newGitCarrierTestHub(t, newBareCarrier(t), "snapshot"))
	assertGitCarrierSweepLock(t, ctx, newGitCarrierTestHub(t, newBareCarrier(t), "sweeplock"))
}

func TestGitCarrierConcurrentPushBothLand(t *testing.T) {
	ctx := context.Background()
	remote := newBareCarrier(t)
	hubA := newGitCarrierTestHub(t, remote, "a")
	hubB := newGitCarrierTestHub(t, remote, "b")
	batchA := []state.Event{
		makeEvent("a1", "dev_a", 10, 1, "project.added", `{"path":"a1"}`),
		makeEvent("a2", "dev_a", 20, 2, "project.added", `{"path":"a2"}`),
	}
	batchB := []state.Event{
		makeEvent("b1", "dev_b", 15, 1, "project.added", `{"path":"b1"}`),
		makeEvent("b2", "dev_b", 25, 2, "project.added", `{"path":"b2"}`),
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs <- hubA.Push(ctx, batchA)
	}()
	go func() {
		defer wg.Done()
		errs <- hubB.Push(ctx, batchB)
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Push: %v", err)
		}
	}
	got, err := newGitCarrierTestHub(t, remote, "reader").Pull(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !sameEventIDs(got, []string{"a1", "b1", "a2", "b2"}) {
		t.Fatalf("Pull ids = %v, want all events from both batches", ids(got))
	}
}

func TestGitCarrierRetentionCASOneWinner(t *testing.T) {
	ctx := context.Background()
	remote := newBareCarrier(t)
	seed := newGitCarrierTestHub(t, remote, "seed")
	if err := seed.PutRetention(ctx, gitCarrierManifestBytes(t, map[string]int64{"dev_a": 1}), ""); err != nil {
		t.Fatal(err)
	}
	hubA := newGitCarrierTestHub(t, remote, "a")
	hubB := newGitCarrierTestHub(t, remote, "b")
	_, etagA, err := hubA.GetRetention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, etagB, err := hubB.GetRetention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	errs := runTwo(func() error {
		return hubA.PutRetention(ctx, gitCarrierManifestBytes(t, map[string]int64{"dev_a": 2}), etagA)
	}, func() error {
		return hubB.PutRetention(ctx, gitCarrierManifestBytes(t, map[string]int64{"dev_a": 3}), etagB)
	})
	assertExactlyOneErr(t, errs, dssync.ErrRetentionConflict)
}

func TestGitCarrierSweepLockOneHolder(t *testing.T) {
	ctx := context.Background()
	remote := newBareCarrier(t)
	hubA := newGitCarrierTestHub(t, remote, "a")
	hubB := newGitCarrierTestHub(t, remote, "b")
	errs := runTwo(func() error {
		return hubA.PutSweepLock(ctx, []byte(`{"holder_device":"dev_a"}`))
	}, func() error {
		return hubB.PutSweepLock(ctx, []byte(`{"holder_device":"dev_b"}`))
	})
	assertExactlyOneErr(t, errs, dssync.ErrSweepLockHeld)
}

func TestGitCarrierCompactSquashesHistory(t *testing.T) {
	ctx := context.Background()
	remote := newBareCarrier(t)
	hubA := newGitCarrierTestHub(t, remote, "a")
	stale := newGitCarrierTestHub(t, remote, "stale")
	events := []state.Event{
		makeEvent("a1", "dev_a", 10, 1, "project.added", "{}"),
		makeEvent("a2", "dev_a", 20, 2, "project.added", "{}"),
		makeEvent("a3", "dev_a", 30, 3, "project.added", "{}"),
		makeEvent("b1", "dev_b", 15, 1, "project.added", "{}"),
	}
	if err := hubA.Push(ctx, events); err != nil {
		t.Fatal(err)
	}
	if _, err := stale.Pull(ctx, nil); err != nil {
		t.Fatal(err)
	}
	deleted, err := hubA.CompactEventsBelow(ctx, dssync.Cursor{"dev_a": 3})
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}
	got, err := newGitCarrierTestHub(t, remote, "fresh").Pull(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !sameEventIDs(got, []string{"b1", "a3"}) {
		t.Fatalf("fresh Pull ids = %v, want surviving events", ids(got))
	}
	if count := gitRevListCount(t, remote, "main"); count != "1" {
		t.Fatalf("git rev-list --count main = %s, want 1", count)
	}
	if err := stale.Push(ctx, []state.Event{makeEvent("stale_new", "dev_stale", 40, 1, "project.added", "{}")}); err != nil {
		t.Fatalf("stale Push after compaction: %v", err)
	}
	got, err = newGitCarrierTestHub(t, remote, "post-stale").Pull(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !sameEventIDs(got, []string{"b1", "a3", "stale_new"}) {
		t.Fatalf("post-stale Pull ids = %v, want survivors plus stale_new", ids(got))
	}
}

func TestGitCarrierRefusesForeignRepo(t *testing.T) {
	ctx := context.Background()
	remote := newBareCarrier(t)
	scratch := filepath.Join(t.TempDir(), "scratch")
	runGit(t, "", "clone", "--quiet", remote, scratch)
	readme := filepath.Join(scratch, "README.md")
	if err := os.WriteFile(readme, []byte("foreign repo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, scratch, "-c", "user.name=test", "-c", "user.email=test@localhost", "add", "README.md")
	runGit(t, scratch, "-c", "user.name=test", "-c", "user.email=test@localhost", "commit", "--quiet", "-m", "foreign")
	runGit(t, scratch, "push", "--quiet", "origin", "HEAD:refs/heads/main")

	h := newGitCarrierTestHub(t, remote, "reader")
	if _, err := h.Pull(ctx, nil); err == nil || !strings.Contains(err.Error(), "marker") {
		t.Fatalf("Pull foreign repo err = %v, want marker refusal", err)
	}
	verify := filepath.Join(t.TempDir(), "verify2")
	runGit(t, "", "clone", "--quiet", remote, verify)
	raw, err := os.ReadFile(filepath.Join(verify, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "foreign repo\n" {
		t.Fatalf("foreign README modified: %q", raw)
	}
}

// TestGitCarrierRealRemoteConformance runs the shared hub contract against a
// real git remote. Set DEVSTRAP_HUB_GIT_TEST_REMOTE to a disposable repository
// URL; the remote repo will receive test data on its main branch.
func TestGitCarrierRealRemoteConformance(t *testing.T) {
	remote := os.Getenv("DEVSTRAP_HUB_GIT_TEST_REMOTE")
	if remote == "" {
		t.Skip("set DEVSTRAP_HUB_GIT_TEST_REMOTE to run the live git-carrier integration test")
	}
	ctx := context.Background()
	assertHubRoundTrip(t, ctx, newGitCarrierTestHub(t, remote, "real"))
}

func runTwo(a func() error, b func() error) []error {
	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs[0] = a()
	}()
	go func() {
		defer wg.Done()
		errs[1] = b()
	}()
	wg.Wait()
	return errs
}

func assertExactlyOneErr(t *testing.T, errs []error, sentinel error) {
	t.Helper()
	var success, matched int
	for _, err := range errs {
		switch {
		case err == nil:
			success++
		case errors.Is(err, sentinel):
			matched++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if success != 1 || matched != 1 {
		t.Fatalf("errors = %v, want exactly one success and one %v", errs, sentinel)
	}
}

func sameEventIDs(events []state.Event, want []string) bool {
	got := ids(events)
	sort.Strings(got)
	sort.Strings(want)
	return strings.Join(got, ",") == strings.Join(want, ",")
}

func gitRevListCount(t *testing.T, remote string, branch string) string {
	t.Helper()
	cmd := exec.Command("git", "--git-dir", remote, "rev-list", "--count", branch)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-list: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}
