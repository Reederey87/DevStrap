package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
)

func newTestR2Hub(t *testing.T) R2Hub {
	t.Helper()
	return R2Hub{S3: newMemS3(), WorkspaceID: "ws_test"}
}

func TestR2EventKeyImmutable(t *testing.T) {
	h := newTestR2Hub(t)
	e := makeEvent("evt_001", "dev_a", 123456, 7, "project.added", `{"path":"x"}`)
	key := h.eventKey(e)
	want := "workspaces/ws_test/eventlog/dev_a/00000000000000000007_evt_001.json"
	if key != want {
		t.Errorf("eventKey = %q, want %q", key, want)
	}
}

func TestR2PushRefusesNonPositiveSeq(t *testing.T) {
	ctx := context.Background()
	h := newTestR2Hub(t)
	e := makeEvent("evt_001", "dev_a", 100, 0, "project.added", `{"path":"x"}`)
	if err := h.Push(ctx, []state.Event{e}); err == nil {
		t.Fatal("Push accepted a Seq<=0 event; the seq-keyed layout cannot represent it")
	}
}

func TestR2PushConcurrentMidBatchFailureSurfaces(t *testing.T) {
	ctx := context.Background()
	mem := newMemS3()
	base := R2Hub{S3: mem, WorkspaceID: "ws_test"}
	events := []state.Event{
		makeEvent("evt_001", "dev_a", 100, 1, "project.added", `{"path":"a"}`),
		makeEvent("evt_002", "dev_a", 200, 2, "project.added", `{"path":"b"}`),
		makeEvent("evt_fail", "dev_a", 300, 3, "project.added", `{"path":"fail"}`),
		makeEvent("evt_004", "dev_a", 400, 4, "project.added", `{"path":"d"}`),
	}
	h := R2Hub{
		S3: &failAfterOtherPutsS3{
			S3Client:    mem,
			failKey:     base.eventKey(events[2]),
			err:         errors.New("forced put failure"),
			wantSuccess: 3,
			ready:       make(chan struct{}),
		},
		WorkspaceID: "ws_test",
	}

	err := h.Push(ctx, events)
	if err == nil {
		t.Fatal("Push: want mid-batch failure, got nil")
	}
	if !strings.Contains(err.Error(), "put event evt_fail: forced put failure") {
		t.Fatalf("Push error = %v, want failing event context", err)
	}
	for _, event := range []state.Event{events[0], events[1], events[3]} {
		if _, err := mem.GetObject(ctx, base.eventKey(event)); err != nil {
			t.Fatalf("successful concurrent put for %s did not land: %v", event.ID, err)
		}
	}
	if _, err := mem.GetObject(ctx, base.eventKey(events[2])); !errors.Is(err, dssync.ErrBlobNotFound) {
		t.Fatalf("failing event object = %v, want ErrBlobNotFound", err)
	}
}

type failAfterOtherPutsS3 struct {
	S3Client
	failKey     string
	err         error
	wantSuccess int
	ready       chan struct{}
	readyOnce   sync.Once
	mu          sync.Mutex
	successes   int
}

func (f *failAfterOtherPutsS3) PutObject(ctx context.Context, key string, body []byte, ifNoneMatch bool) error {
	if key == f.failKey {
		select {
		case <-f.ready:
		case <-time.After(time.Second):
		}
		return f.err
	}
	err := f.S3Client.PutObject(ctx, key, body, ifNoneMatch)
	if err == nil {
		f.mu.Lock()
		f.successes++
		if f.successes >= f.wantSuccess {
			f.readyOnce.Do(func() { close(f.ready) })
		}
		f.mu.Unlock()
	}
	return err
}

func TestR2PushConcurrentBatchLandsAllEvents(t *testing.T) {
	ctx := context.Background()
	h := newTestR2Hub(t)
	events := make([]state.Event, 0, 50)
	for i := 0; i < 50; i++ {
		events = append(events, makeEvent(
			fmt.Sprintf("evt_%03d", i),
			"dev_a",
			int64(100+i),
			int64(i+1),
			"project.added",
			fmt.Sprintf(`{"path":"p%d"}`, i),
		))
	}
	if err := h.Push(ctx, events); err != nil {
		t.Fatalf("Push: %v", err)
	}
	for _, event := range events {
		raw, err := h.S3.GetObject(ctx, h.eventKey(event))
		if err != nil {
			t.Fatalf("GetObject(%s): %v", event.ID, err)
		}
		want, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("Marshal(%s): %v", event.ID, err)
		}
		if !bytes.Equal(raw, want) {
			t.Fatalf("object bytes for %s = %s, want %s", event.ID, raw, want)
		}
	}
}

// assertHubRoundTrip runs the hub conformance contract (P5-HUB-01) against any
// R2Hub/S3Client pair: the event-log plane (push/pull order, HLC cursor with
// inclusive boundary, conditional-put idempotency) and the blob plane
// (content-addressed put/get, not-found, ListBlobs, idempotent delete, invalid
// key). It is shared by the in-memory memS3 test and the live MinIO/R2 test so
// the production aws-sdk-go-v2 adapter is proven against the SAME contract as
// the conformance double. Retention-floor, skip-HEAD, pagination, and retry
// behaviors are memS3/fault-injection-specific and stay in their own tests below.
func assertHubRoundTrip(t *testing.T, ctx context.Context, h dssync.Hub) {
	t.Helper()

	// Event-log plane: push/pull round trip + deterministic (hlc, device, id) order.
	events := []state.Event{
		makeEvent("evt_001", "dev_a", 100, 1, "project.added", `{"path":"a"}`),
		makeEvent("evt_002", "dev_b", 200, 1, "project.added", `{"path":"b"}`),
		makeEvent("evt_003", "dev_a", 150, 2, "project.updated", `{"path":"a"}`),
	}
	if err := h.Push(ctx, events); err != nil {
		t.Fatalf("Push: %v", err)
	}
	pulled, err := h.Pull(ctx, nil)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(pulled) != 3 {
		t.Fatalf("Pull returned %d events, want 3", len(pulled))
	}
	if pulled[0].HLC != 100 || pulled[1].HLC != 150 || pulled[2].HLC != 200 {
		t.Errorf("Pull order: HLCs = %d %d %d, want 100 150 200", pulled[0].HLC, pulled[1].HLC, pulled[2].HLC)
	}

	// EAGER-02/P5-SYNC-01 per-device Seq cursor: the boundary is exact (no
	// HUB-13 overlap re-delivery), and each device's stream resumes
	// independently.
	pulled, err = h.Pull(ctx, dssync.Cursor{"dev_a": 1})
	if err != nil {
		t.Fatalf("Pull(dev_a:1): %v", err)
	}
	if len(pulled) != 2 {
		t.Errorf("Pull(dev_a:1) = %d events, want 2 (dev_a seq 2 + dev_b seq 1)", len(pulled))
	}
	pulled, err = h.Pull(ctx, dssync.Cursor{"dev_a": 2})
	if err != nil {
		t.Fatalf("Pull(dev_a:2): %v", err)
	}
	if len(pulled) != 1 || pulled[0].ID != "evt_002" {
		t.Errorf("Pull(dev_a:2) = %v, want only dev_b's evt_002", pulled)
	}
	pulled, err = h.Pull(ctx, dssync.Cursor{"dev_a": 2, "dev_b": 1})
	if err != nil {
		t.Fatalf("Pull(both consumed): %v", err)
	}
	if len(pulled) != 0 {
		t.Errorf("Pull(both consumed) = %d events, want 0", len(pulled))
	}
	// P5-SYNC-01 the defect scenario at hub level: an event pushed LATE with
	// an HLC (and seq) far below another device's already-consumed positions
	// is still delivered — the per-device cursor cannot skip it.
	late := makeEvent("evt_late", "dev_c", 50, 1, "project.added", `{"path":"late"}`)
	if err := h.Push(ctx, []state.Event{late}); err != nil {
		t.Fatalf("Push late: %v", err)
	}
	pulled, err = h.Pull(ctx, dssync.Cursor{"dev_a": 2, "dev_b": 1})
	if err != nil {
		t.Fatalf("Pull after late push: %v", err)
	}
	if len(pulled) != 1 || pulled[0].ID != "evt_late" {
		t.Errorf("late-pushed old-HLC event not delivered: got %v", pulled)
	}

	// HUB-06: re-pushing the same event is a no-op (conditional put dedup).
	if err := h.Push(ctx, []state.Event{events[0]}); err != nil {
		t.Fatalf("re-Push (dup): %v", err)
	}
	pulled, _ = h.Pull(ctx, nil)
	if len(pulled) != 4 {
		t.Errorf("after duplicate push, got %d events, want 4 (3 originals + the late push)", len(pulled))
	}

	// Blob plane: content-addressed put/get is idempotent and byte-faithful.
	keyA := strings.Repeat("a", 64)
	keyB := strings.Repeat("b", 64)
	data := []byte("encrypted-blob-content")
	if err := h.PutBlob(ctx, keyA, bytes.NewReader(data)); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	if err := h.PutBlob(ctx, keyA, bytes.NewReader(data)); err != nil {
		t.Fatalf("re-PutBlob (idempotent): %v", err)
	}
	rc, err := h.GetBlob(ctx, keyA)
	if err != nil {
		t.Fatalf("GetBlob: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, data) {
		t.Errorf("GetBlob = %q, want %q", got, data)
	}

	// A never-uploaded blob is not found (wraps dssync.ErrBlobNotFound).
	if _, err := h.GetBlob(ctx, keyB); !errors.Is(err, dssync.ErrBlobNotFound) {
		t.Errorf("GetBlob missing = %v, want ErrBlobNotFound", err)
	}

	// P5-HUB-02: ListBlobs enumerates the workspace's blobs by sha256 hex key.
	if err := h.PutBlob(ctx, keyB, bytes.NewReader([]byte("two"))); err != nil {
		t.Fatalf("PutBlob keyB: %v", err)
	}
	listed, err := h.ListBlobs(ctx)
	if err != nil {
		t.Fatalf("ListBlobs: %v", err)
	}
	sort.Slice(listed, func(i, j int) bool { return listed[i].Key < listed[j].Key })
	if len(listed) != 2 || listed[0].Key != keyA || listed[1].Key != keyB {
		t.Errorf("ListBlobs = %v, want [%s %s]", listed, keyA, keyB)
	}
	for _, blob := range listed {
		if blob.LastModified.IsZero() {
			t.Errorf("ListBlobs returned zero LastModified for %s", blob.Key)
		}
	}

	// SEC-01/HUB-12: DeleteBlob removes a blob and is idempotent on a missing blob
	// so revoke/GC can call it unconditionally for superseded ciphertext.
	if err := h.DeleteBlob(ctx, keyA); err != nil {
		t.Fatalf("DeleteBlob: %v", err)
	}
	if _, err := h.GetBlob(ctx, keyA); !errors.Is(err, dssync.ErrBlobNotFound) {
		t.Errorf("GetBlob after delete = %v, want ErrBlobNotFound", err)
	}
	if err := h.DeleteBlob(ctx, keyA); err != nil {
		t.Fatalf("idempotent delete of missing blob: %v", err)
	}
	if err := h.DeleteBlob(ctx, strings.Repeat("c", 64)); err != nil {
		t.Fatalf("idempotent delete of never-existing blob: %v", err)
	}

	// Invalid blob key is rejected.
	if err := h.PutBlob(ctx, "short", bytes.NewReader([]byte("x"))); err == nil {
		t.Error("expected error for invalid blob key")
	}
}

// TestR2ConformanceMemS3 runs the shared conformance contract against the
// in-memory double (HUB-02). The same assertHubRoundTrip is run against a live
// MinIO/R2 bucket in r2_minio_test.go, proving the production adapter matches
// the conformance double.
func TestR2ConformanceMemS3(t *testing.T) {
	assertHubRoundTrip(t, context.Background(), newTestR2Hub(t))
}

func TestR2WorkspacePrefixIsolation(t *testing.T) {
	ctx := context.Background()
	shared := newMemS3()
	founder := R2Hub{S3: shared, WorkspaceID: "ws_founder"}
	paired := R2Hub{S3: shared, WorkspaceID: "ws_founder"}
	other := R2Hub{S3: shared, WorkspaceID: "ws_other"}

	events := []state.Event{
		makeEvent("evt_001", "dev_a", 100, 1, "project.added", `{"path":"a"}`),
		makeEvent("evt_002", "dev_a", 200, 2, "project.updated", `{"path":"a"}`),
	}
	if err := founder.Push(ctx, events); err != nil {
		t.Fatalf("founder Push: %v", err)
	}
	blobKey := strings.Repeat("c", 64)
	blobData := []byte("encrypted-blob-content")
	if err := founder.PutBlob(ctx, blobKey, bytes.NewReader(blobData)); err != nil {
		t.Fatalf("founder PutBlob: %v", err)
	}

	pairedEvents, err := paired.Pull(ctx, nil)
	if err != nil {
		t.Fatalf("paired Pull: %v", err)
	}
	if len(pairedEvents) != len(events) {
		t.Errorf("paired Pull returned %d events, want %d", len(pairedEvents), len(events))
	} else {
		for i := range events {
			if pairedEvents[i].ID != events[i].ID {
				t.Errorf("paired Pull event[%d] = %s, want %s", i, pairedEvents[i].ID, events[i].ID)
			}
		}
	}
	rc, err := paired.GetBlob(ctx, blobKey)
	if err != nil {
		t.Fatalf("paired GetBlob: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, blobData) {
		t.Errorf("paired GetBlob = %q, want %q", got, blobData)
	}

	otherEvents, err := other.Pull(ctx, nil)
	if err != nil {
		t.Fatalf("other Pull: %v", err)
	}
	if len(otherEvents) != 0 {
		t.Errorf("other Pull returned %d events, want 0", len(otherEvents))
	}
	if _, err := other.GetBlob(ctx, blobKey); !errors.Is(err, dssync.ErrBlobNotFound) {
		t.Errorf("other GetBlob = %v, want ErrBlobNotFound", err)
	}

	hasEvents, err := paired.HasEvents(ctx)
	if err != nil {
		t.Fatalf("paired HasEvents: %v", err)
	}
	if !hasEvents {
		t.Error("paired HasEvents = false, want true")
	}
	hasEvents, err = other.HasEvents(ctx)
	if err != nil {
		t.Fatalf("other HasEvents: %v", err)
	}
	if hasEvents {
		t.Error("other HasEvents = true, want false")
	}
}

// P5-HUB-03 (Seq-re-based): a Pull whose per-device cursor would leave a gap
// below that device's retention floor must return ErrSnapshotRequired instead
// of a silently-incomplete (post-compaction) set.
func TestR2HubPullRetentionFloor(t *testing.T) {
	ctx := context.Background()
	h := newTestR2Hub(t)
	h.RetentionSeqs = map[string]int64{"dev_a": 5}
	if err := h.Push(ctx, []state.Event{makeEvent("evt_1", "dev_a", 150, 6, "project.added", `{"path":"a"}`)}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	// Cursor below the floor (next needed seq 3 < min retained 5) → snapshot.
	if _, err := h.Pull(ctx, dssync.Cursor{"dev_a": 2}); !errors.Is(err, dssync.ErrSnapshotRequired) {
		t.Fatalf("Pull(dev_a:2) = %v, want ErrSnapshotRequired", err)
	}
	// Cursor exactly at the floor boundary (next needed seq 5 == floor) →
	// normal incremental pull.
	pulled, err := h.Pull(ctx, dssync.Cursor{"dev_a": 4})
	if err != nil {
		t.Fatalf("Pull(dev_a:4): %v", err)
	}
	if len(pulled) != 1 {
		t.Fatalf("Pull(dev_a:4) = %d events, want 1", len(pulled))
	}
}

// P5-SYNC-01: the legacy HLC-keyed layout stays readable (dual-read), the
// per-device cursor applies to parsed legacy keys, unparseable legacy keys
// fail open toward fetching, and events present under both layouts dedup.
func TestR2HubPullLegacyLayoutDualRead(t *testing.T) {
	ctx := context.Background()
	h := newTestR2Hub(t)
	mem := h.S3.(*memS3)
	legacy := func(e state.Event) string {
		raw, _ := json.Marshal(e)
		key := fmt.Sprintf("workspaces/%s/events/%020d/%s/%d/%s.json", h.WorkspaceID, e.HLC, e.DeviceID, e.Seq, e.ID)
		if err := mem.PutObject(ctx, key, raw, true); err != nil {
			t.Fatalf("seed legacy object: %v", err)
		}
		return key
	}
	eA1 := makeEvent("evt_a1", "dev_a", 100, 1, "project.added", `{"path":"a"}`)
	eA2 := makeEvent("evt_a2", "dev_a", 200, 2, "project.updated", `{"path":"a"}`)
	legacy(eA1)
	legacy(eA2)
	// An unparseable legacy key (missing seq segment) must still be fetched.
	oddball := makeEvent("evt_odd", "dev_b", 150, 1, "project.added", `{"path":"b"}`)
	rawOdd, _ := json.Marshal(oddball)
	if err := mem.PutObject(ctx, fmt.Sprintf("workspaces/%s/events/strange-key.json", h.WorkspaceID), rawOdd, true); err != nil {
		t.Fatalf("seed oddball: %v", err)
	}
	// A new-layout copy of eA2 (as a future migrate-events would create) must
	// dedup with its legacy twin.
	if err := h.Push(ctx, []state.Event{eA2}); err != nil {
		t.Fatalf("Push new-layout twin: %v", err)
	}

	pulled, err := h.Pull(ctx, nil)
	if err != nil {
		t.Fatalf("Pull(nil): %v", err)
	}
	if len(pulled) != 3 {
		t.Fatalf("Pull(nil) = %d events, want 3 (a1, odd, a2 — deduped)", len(pulled))
	}
	// The per-device cursor prunes parsed legacy keys: dev_a consumed through
	// seq 1 → only a2 (and the unparseable-key event, seq-filtered after
	// fetch by its own Seq > cursor) remain.
	pulled, err = h.Pull(ctx, dssync.Cursor{"dev_a": 1})
	if err != nil {
		t.Fatalf("Pull(dev_a:1): %v", err)
	}
	var ids []string
	for _, e := range pulled {
		ids = append(ids, e.ID)
	}
	if len(pulled) != 2 || pulled[0].ID != "evt_odd" || pulled[1].ID != "evt_a2" {
		t.Fatalf("Pull(dev_a:1) ids = %v, want [evt_odd evt_a2]", ids)
	}
	// A legacy-only late push (an old binary writing the retired layout) is
	// still caught by the per-device cursor logic.
	lateOld := makeEvent("evt_late_legacy", "dev_c", 10, 1, "project.added", `{"path":"c"}`)
	legacy(lateOld)
	pulled, err = h.Pull(ctx, dssync.Cursor{"dev_a": 2, "dev_b": 1})
	if err != nil {
		t.Fatalf("Pull after legacy late push: %v", err)
	}
	if len(pulled) != 1 || pulled[0].ID != "evt_late_legacy" {
		t.Fatalf("legacy late push not delivered: %v", pulled)
	}
}

// P5-SYNC-01: device-stream discovery lists the eventlog prefix with a
// delimiter, so a brand-new device's stream is pulled with no cursor entry.
func TestR2HubDeviceDiscovery(t *testing.T) {
	ctx := context.Background()
	h := newTestR2Hub(t)
	if err := h.Push(ctx, []state.Event{
		makeEvent("evt_a1", "dev_a", 100, 1, "t", `{}`),
		makeEvent("evt_b1", "dev_b", 200, 1, "t", `{}`),
	}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	pulled, err := h.Pull(ctx, dssync.Cursor{"dev_a": 1})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(pulled) != 1 || pulled[0].ID != "evt_b1" {
		t.Fatalf("unknown device's stream not discovered: %v", pulled)
	}
}

// countS3 wraps an S3Client and counts ObjectExists (HEAD) and PutObject calls
// so tests can assert HUB-09: the write path must not issue a redundant
// ObjectExists before the conditional put.
type countS3 struct {
	S3Client
	heads int
	puts  int
}

func (c *countS3) PutObject(ctx context.Context, key string, body []byte, ifNoneMatch bool) error {
	c.puts++
	return c.S3Client.PutObject(ctx, key, body, ifNoneMatch)
}

func (c *countS3) ObjectExists(ctx context.Context, key string) (bool, error) {
	c.heads++
	return c.S3Client.ObjectExists(ctx, key)
}

// TestR2WritePathSkipsObjectExists (HUB-09): Push and PutBlob rely solely on
// the conditional put and never issue a redundant ObjectExists/HEAD, even when
// re-pushing a duplicate event or blob. A 412 PreconditionFailed is classified
// as an idempotent dedup hit.
func TestR2WritePathSkipsObjectExists(t *testing.T) {
	ctx := context.Background()
	c := &countS3{S3Client: newMemS3()}
	h := R2Hub{S3: c, WorkspaceID: "ws_test"}
	e := makeEvent("evt_001", "dev_a", 100, 1, "project.added", `{"path":"a"}`)
	if err := h.Push(ctx, []state.Event{e}); err != nil {
		t.Fatalf("Push 1: %v", err)
	}
	// Re-pushing the same event is an idempotent no-op via 412, no HEAD.
	if err := h.Push(ctx, []state.Event{e}); err != nil {
		t.Fatalf("Push 2 (dup): %v", err)
	}
	if c.heads != 0 {
		t.Errorf("Push issued %d ObjectExists calls, want 0 (HUB-09)", c.heads)
	}
	blob := []byte("encrypted-blob-content")
	if err := h.PutBlob(ctx, strings.Repeat("a", 64), bytes.NewReader(blob)); err != nil {
		t.Fatalf("PutBlob 1: %v", err)
	}
	if err := h.PutBlob(ctx, strings.Repeat("a", 64), bytes.NewReader(blob)); err != nil {
		t.Fatalf("PutBlob 2 (dup): %v", err)
	}
	if c.heads != 0 {
		t.Errorf("PutBlob issued %d ObjectExists calls, want 0 (HUB-09)", c.heads)
	}
}

func TestR2Pagination(t *testing.T) {
	ctx := context.Background()
	h := newTestR2Hub(t)
	// Push enough events to exercise pagination (maxKeys=1000 in R2Hub).
	for i := 0; i < 5; i++ {
		e := makeEvent(
			"evt_"+string(rune('a'+i)),
			"dev_a",
			int64(100+i),
			int64(i+1),
			"project.added",
			`{"path":"x"}`,
		)
		if err := h.Push(ctx, []state.Event{e}); err != nil {
			t.Fatalf("Push %d: %v", i, err)
		}
	}
	// Force small page size by directly using the S3 client.
	prefix := h.eventlogPrefix()
	objs, _, err := h.S3.ListObjectsV2(ctx, prefix, "", 3)
	if err != nil {
		t.Fatalf("ListObjectsV2: %v", err)
	}
	if len(objs) != 3 {
		t.Errorf("page size = %d, want 3", len(objs))
	}
}

// faultS3 wraps an S3Client and injects a configured error on the first failN
// PutObject calls, then delegates. Used to exercise HUB-10 retry/recovery.
type faultS3 struct {
	S3Client
	mu     sync.Mutex
	calls  int
	failN  int
	inject error
}

func (f *faultS3) PutObject(ctx context.Context, key string, body []byte, ifNoneMatch bool) error {
	f.mu.Lock()
	f.calls++
	n := f.calls
	failN := f.failN
	inject := f.inject
	f.mu.Unlock()
	if n <= failN {
		return inject
	}
	return f.S3Client.PutObject(ctx, key, body, ifNoneMatch)
}

func (f *faultS3) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func fastRetry() R2Retry {
	return R2Retry{
		MaxAttempts:   3,
		BaseDelay:     time.Millisecond,
		ThrottleDelay: time.Millisecond,
		Cap:           10 * time.Millisecond,
		Jitter:        func(int64) int64 { return 0 },
	}
}

// TestR2PushRetriesThrottling (HUB-10): a throttling error on the first two
// PutObject attempts is retried with backoff and the third attempt succeeds.
func TestR2PushRetriesThrottling(t *testing.T) {
	ctx := context.Background()
	f := &faultS3{S3Client: newMemS3(), failN: 2, inject: ErrS3Throttle}
	h := R2Hub{S3: f, WorkspaceID: "ws_test", Retry: fastRetry()}
	e := makeEvent("evt_001", "dev_a", 100, 1, "project.added", `{"path":"a"}`)
	if err := h.Push(ctx, []state.Event{e}); err != nil {
		t.Fatalf("Push: %v, want retry recovery", err)
	}
	if f.CallCount() < 3 {
		t.Fatalf("PutObject calls = %d, want >=3 (retried)", f.CallCount())
	}
}

// TestR2PushRetriesTransient (HUB-10): a transient error is retried and recovers.
func TestR2PushRetriesTransient(t *testing.T) {
	ctx := context.Background()
	f := &faultS3{S3Client: newMemS3(), failN: 1, inject: ErrS3Transient}
	h := R2Hub{S3: f, WorkspaceID: "ws_test", Retry: fastRetry()}
	e := makeEvent("evt_001", "dev_a", 100, 1, "project.added", `{"path":"a"}`)
	if err := h.Push(ctx, []state.Event{e}); err != nil {
		t.Fatalf("Push: %v, want retry recovery", err)
	}
	if f.CallCount() < 2 {
		t.Fatalf("PutObject calls = %d, want >=2 (retried)", f.CallCount())
	}
}

// TestR2PushDoesNotRetryTerminal (HUB-10): an unclassified/terminal error (e.g.
// auth) is not retried; Push fails fast on the first attempt.
func TestR2PushDoesNotRetryTerminal(t *testing.T) {
	ctx := context.Background()
	f := &faultS3{S3Client: newMemS3(), failN: 5, inject: errors.New("s3 auth error")}
	h := R2Hub{S3: f, WorkspaceID: "ws_test", Retry: fastRetry()}
	err := h.Push(ctx, []state.Event{makeEvent("evt_001", "dev_a", 100, 1, "project.added", `{"path":"a"}`)})
	if err == nil {
		t.Fatal("Push: want terminal error, got nil")
	}
	if f.CallCount() != 1 {
		t.Fatalf("PutObject calls = %d, want 1 (no retry on terminal)", f.CallCount())
	}
}

// TestR2RetryRespectsContextCancellation (HUB-10): a cancelled context aborts
// the retry loop rather than sleeping through the backoff.
func TestR2RetryRespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := R2Retry{MaxAttempts: 3, BaseDelay: time.Minute, ThrottleDelay: time.Minute, Jitter: func(int64) int64 { return 0 }}
	err := r.do(ctx, func() error { return ErrS3Throttle })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("do with cancelled ctx = %v, want context.Canceled", err)
	}
}
