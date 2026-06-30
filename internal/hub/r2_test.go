package hub

import (
	"bytes"
	"context"
	"errors"
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
	e := makeEvent("evt_001", "dev_a", 123456, 1, "project.added", `{"path":"x"}`)
	key := h.eventKey(e)
	want := "workspaces/ws_test/events/00000000000000123456/dev_a/1/evt_001.json"
	if key != want {
		t.Errorf("eventKey = %q, want %q", key, want)
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
func assertHubRoundTrip(t *testing.T, h R2Hub) {
	t.Helper()
	ctx := context.Background()

	// Event-log plane: push/pull round trip + deterministic (hlc, device, id) order.
	events := []state.Event{
		makeEvent("evt_001", "dev_a", 100, 1, "project.added", `{"path":"a"}`),
		makeEvent("evt_002", "dev_b", 200, 1, "project.added", `{"path":"b"}`),
		makeEvent("evt_003", "dev_a", 150, 2, "project.updated", `{"path":"a"}`),
	}
	if err := h.Push(ctx, events); err != nil {
		t.Fatalf("Push: %v", err)
	}
	pulled, err := h.Pull(ctx, 0)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(pulled) != 3 {
		t.Fatalf("Pull returned %d events, want 3", len(pulled))
	}
	if pulled[0].HLC != 100 || pulled[1].HLC != 150 || pulled[2].HLC != 200 {
		t.Errorf("Pull order: HLCs = %d %d %d, want 100 150 200", pulled[0].HLC, pulled[1].HLC, pulled[2].HLC)
	}

	// EAGER-02 cursor-based pull + HUB-13 inclusive boundary: Pull(afterHLC)
	// returns events with HLC >= afterHLC, so a same-HLC late arrival is not dropped.
	pulled, err = h.Pull(ctx, 150)
	if err != nil {
		t.Fatalf("Pull(150): %v", err)
	}
	if len(pulled) != 2 {
		t.Errorf("Pull(150) = %d events, want 2 (inclusive boundary + newer)", len(pulled))
	}
	pulled, err = h.Pull(ctx, 200)
	if err != nil {
		t.Fatalf("Pull(200): %v", err)
	}
	if len(pulled) != 1 || pulled[0].ID != "evt_002" {
		t.Errorf("Pull(200) = %v, want the boundary evt_002 (inclusive)", pulled)
	}
	pulled, err = h.Pull(ctx, 201)
	if err != nil {
		t.Fatalf("Pull(201): %v", err)
	}
	if len(pulled) != 0 {
		t.Errorf("Pull(201) = %d events, want 0", len(pulled))
	}

	// HUB-06: re-pushing the same event is a no-op (conditional put dedup).
	if err := h.Push(ctx, []state.Event{events[0]}); err != nil {
		t.Fatalf("re-Push (dup): %v", err)
	}
	pulled, _ = h.Pull(ctx, 0)
	if len(pulled) != 3 {
		t.Errorf("after duplicate push, got %d events, want 3", len(pulled))
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
	sort.Strings(listed)
	if len(listed) != 2 || listed[0] != keyA || listed[1] != keyB {
		t.Errorf("ListBlobs = %v, want [%s %s]", listed, keyA, keyB)
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
	assertHubRoundTrip(t, newTestR2Hub(t))
}

// P5-HUB-03: a Pull whose cursor is below the retention horizon must return
// ErrSnapshotRequired instead of a silently-incomplete (post-compaction) set.
func TestR2HubPullRetentionFloor(t *testing.T) {
	ctx := context.Background()
	h := newTestR2Hub(t)
	h.RetentionHLC = 100
	if err := h.Push(ctx, []state.Event{makeEvent("evt_1", "dev_a", 150, 1, "project.added", `{"path":"a"}`)}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	// Below the floor → snapshot required.
	if _, err := h.Pull(ctx, 50); !errors.Is(err, dssync.ErrSnapshotRequired) {
		t.Fatalf("Pull(50) = %v, want ErrSnapshotRequired", err)
	}
	// At/above the floor → normal incremental pull.
	pulled, err := h.Pull(ctx, 100)
	if err != nil {
		t.Fatalf("Pull(100): %v", err)
	}
	if len(pulled) != 1 {
		t.Fatalf("Pull(100) = %d events, want 1", len(pulled))
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
	prefix := h.eventsPrefix()
	keys, _, err := h.S3.ListObjectsV2(ctx, prefix, "", 3)
	if err != nil {
		t.Fatalf("ListObjectsV2: %v", err)
	}
	if len(keys) != 3 {
		t.Errorf("page size = %d, want 3", len(keys))
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
