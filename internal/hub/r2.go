// R2 is S3-compatible with zero egress, strong consistency for object
// writes/listing, and conditional puts. Event-log payloads are
// envelope-encrypted (XChaCha20-Poly1305 under a per-epoch Workspace Content
// Key, P4-SEC-02/SEC-07) and Ed25519-signed before upload; blobs are
// age-encrypted. R2 stores only ciphertext plus a signed carrier map — it can
// decrypt nothing and holds no private key.
//
// The event log is NOT one overwritten manifest object. Every event is an
// immutable, unique, lexicographically sortable object (HUB-06). Since
// P5-SYNC-01 the transport coordinate is the per-origin-device sequence
// number, so Push writes per-device, seq-ordered keys:
//
//	workspaces/<workspace_id>/eventlog/<device_id>/<seq-padded>_<event_id>.json
//
// and Pull resumes each device's stream with a per-device Seq cursor
// (delimiter-discovered device prefixes + StartAfter within each). The retired
// HLC-keyed legacy layout
//
//	workspaces/<workspace_id>/events/<hlc-padded>/<device_id>/<seq>/<event_id>.json
//
// is still READ (dual-read) so pre-migration hubs keep working — Pull parses
// (device, seq) out of legacy keys and applies the same per-device cursor;
// unparseable legacy keys fail open toward fetching so a parse bug can never
// silently lose events. Push never writes the legacy layout. A follow-up
// `hub migrate-events` can re-key legacy objects and delete the old prefix;
// the dual-read is O(1) on an empty prefix.
//
// Blobs are content-addressed (HUB-06):
//
//	workspaces/<workspace_id>/blobs/<sha256>
//
// The S3 operations are abstracted behind the S3Client interface so the keying
// scheme and Hub contract are unit-testable with an in-memory double (the
// conformance suite). A real implementation plugs in the AWS SDK v2 S3 client.
package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
	"golang.org/x/sync/errgroup"
)

// r2PullConcurrency bounds how many event objects R2Hub.Pull fetches in
// parallel (P5-HUB-04). It mirrors the materialize pass's bounded fan-out so a
// cold-start pull of thousands of events is not O(events) serial round-trips,
// without exhausting connections.
const r2PullConcurrency = 8

// S3Client is the minimal S3-compatible operation set the R2 backend needs
// (HUB-02). It is abstracted so the keying scheme and Hub contract are
// testable with an in-memory double. A production implementation wraps the
// AWS SDK v2 S3 client pointed at an R2 endpoint.
type S3Client interface {
	// PutObject stores data at key. When ifNoneMatch is true the put is
	// conditional on the object not already existing (If-None-Match: *),
	// making event append idempotent (HUB-06).
	PutObject(ctx context.Context, key string, body []byte, ifNoneMatch bool) error
	// GetObject returns the object at key, or an error wrapping
	// dssync.ErrBlobNotFound when absent.
	GetObject(ctx context.Context, key string) ([]byte, error)
	// ObjectExists reports whether an object exists at key.
	ObjectExists(ctx context.Context, key string) (bool, error)
	// DeleteObject removes the object at key. A missing object is not an error
	// (idempotent delete) so blob/event GC (HUB-12) and revoke cleanup (SEC-01)
	// can call it unconditionally for superseded ciphertext.
	DeleteObject(ctx context.Context, key string) error
	// ListObjectsV2 returns objects under prefix, lexicographically after
	// startAfter, up to maxKeys. BlobInfo is reused as a generic key+time pair;
	// for event objects, Key is the full trimmed key as before. When truncated,
	// it returns the next key to continue from.
	ListObjectsV2(ctx context.Context, prefix, startAfter string, maxKeys int) (objs []dssync.BlobInfo, nextStartAfter string, err error)
	// ListCommonPrefixes returns the distinct sub-prefixes directly under
	// prefix, grouped at the given delimiter (ListObjectsV2 CommonPrefixes).
	// P5-SYNC-01 uses it to discover origin-device streams under the eventlog/
	// prefix without listing every object.
	ListCommonPrefixes(ctx context.Context, prefix, delimiter string) ([]string, error)
}

// R2Hub is the Cloudflare R2 zero-knowledge Hub backend (HUB-02). It implements
// dssync.Hub. All content is client-side encrypted before it reaches S3Client.
type R2Hub struct {
	S3          S3Client
	WorkspaceID string
	// RetentionSeqs is the hub's per-device retention horizon (P5-HUB-03,
	// re-based on the P5-SYNC-01 Seq cursor): for each origin device, the
	// minimum Seq still retained on the hub. A Pull whose cursor would leave a
	// gap below a device's floor (after[dev]+1 < RetentionSeqs[dev]) returns
	// dssync.ErrSnapshotRequired so the caller performs a full-state snapshot
	// exchange instead of silently receiving a partial (post-compaction) event
	// set and diverging. Empty means "no compaction yet" (R2 retains
	// everything). The future compaction marker must be recorded per device
	// (e.g. eventlog/<device>/.retention).
	RetentionSeqs map[string]int64
	// Retry configures R2Hub-level retry, backoff, and error classification for
	// S3 operations (HUB-10). A zero value uses a default policy: throttling
	// (429/503 SlowDown) and transient (500/connection-reset) errors are retried
	// with capped exponential backoff plus full jitter; terminal errors (auth,
	// precondition, not-found, malformed) fail fast. A real aws-sdk-go-v2
	// client wires its own standard retryer; this seam works with any S3Client
	// (including the in-memory conformance double) and is exercised via fault
	// injection before the SDK is wired.
	Retry R2Retry
}

// retry returns the effective retry policy, defaulting a zero-value R2Retry
// (HUB-10).
func (h R2Hub) retry() R2Retry {
	return h.Retry.policy()
}

// Compile-time assertion that R2Hub satisfies dssync.Hub (HUB-01/HUB-02).
var _ dssync.Hub = R2Hub{}

// eventKey builds the immutable, per-device seq-ordered object key
// (HUB-06/P5-SYNC-01). The seq is zero-padded to 20 digits so lexical ordering
// within a device prefix matches numeric ordering; the event_id suffix keeps
// the key unique per event, so a divergent same-seq re-mint creates a second
// object (both are fetched and the divergence quarantines locally) while an
// exact duplicate re-push is a conditional-PUT dedup no-op.
func (h R2Hub) eventKey(event state.Event) string {
	return fmt.Sprintf("%s%020d_%s.json", h.devicePrefix(event.DeviceID), event.Seq, event.ID)
}

// blobKey builds the content-addressed object key for an encrypted blob
// (HUB-06).
func (h R2Hub) blobKey(sha256Hex string) string {
	return fmt.Sprintf("workspaces/%s/blobs/%s", h.WorkspaceID, sha256Hex)
}

// eventlogPrefix is the seq-keyed event layout root (P5-SYNC-01). Device
// streams live directly under it, one prefix per origin device.
func (h R2Hub) eventlogPrefix() string {
	return fmt.Sprintf("workspaces/%s/eventlog/", h.WorkspaceID)
}

// devicePrefix is one origin device's stream prefix in the seq-keyed layout.
func (h R2Hub) devicePrefix(deviceID string) string {
	return h.eventlogPrefix() + deviceID + "/"
}

// legacyEventsPrefix is the retired HLC-keyed layout (pre-P5-SYNC-01),
// dual-READ only: Push never writes it; Pull parses (device, seq) out of its
// keys so pre-migration hubs keep working under the per-device cursor.
func (h R2Hub) legacyEventsPrefix() string {
	return fmt.Sprintf("workspaces/%s/events/", h.WorkspaceID)
}

func (h R2Hub) Push(ctx context.Context, events []state.Event) error {
	for _, event := range events {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// P5-SYNC-01: the seq-keyed layout is meaningless for a pre-sequence
		// event — a Seq <= 0 key would sort as seq 0 and break the per-device
		// cursor contract. Local stamping has assigned Seq since migration
		// 00002, so this only fires on a programming error.
		if event.Seq <= 0 {
			return fmt.Errorf("refusing to push event %s with non-positive seq %d", event.ID, event.Seq)
		}
		key := h.eventKey(event)
		raw, err := json.Marshal(event)
		if err != nil {
			return fmt.Errorf("marshal event %s: %w", event.ID, err)
		}
		// HUB-06/HUB-09: the conditional put (If-None-Match: *) is itself the
		// atomic, idempotent guard for event append, so no separate
		// ObjectExists/HEAD is needed. Dropping the HEAD halves the per-event
		// request count on the hot push path and removes the check-then-act
		// race a concurrent writer could win between the HEAD and the PUT. A
		// 412 PreconditionFailed is a duplicate event and a no-op. HUB-10:
		// throttling/transient S3 errors are retried with backoff; a 412 is
		// terminal (not retried) and handled as a dedup hit below.
		if err := h.retry().do(ctx, func() error { return h.S3.PutObject(ctx, key, raw, true) }); err != nil {
			if errors.Is(err, ErrPreconditionFailed) {
				continue // idempotent dedup hit
			}
			return fmt.Errorf("put event %s: %w", event.ID, err)
		}
	}
	return nil
}

func (h R2Hub) Pull(ctx context.Context, after dssync.Cursor) ([]state.Event, error) {
	// P5-HUB-03 (Seq-re-based): honor the per-device retention horizon. A
	// cursor below any device's compaction floor means the incremental log has
	// a gap only a full-state snapshot exchange can fill.
	for dev, minRetained := range h.RetentionSeqs {
		if minRetained > 0 && after.After(dev)+1 < minRetained {
			return nil, dssync.ErrSnapshotRequired
		}
	}
	// P5-SYNC-01: discover origin-device streams in the seq-keyed layout, then
	// resume each with StartAfter on its padded seq boundary. The bare padded
	// target (no "_<id>" suffix) sorts BELOW every real key at that seq, so
	// StartAfter = <prefix><pad20(after+1)> excludes all of seq `after` and
	// includes all of seq after+1 — an exact boundary, no overlap re-delivery.
	var devPrefixes []string
	if err := h.retry().do(ctx, func() error {
		var lerr error
		devPrefixes, lerr = h.S3.ListCommonPrefixes(ctx, h.eventlogPrefix(), "/")
		return lerr
	}); err != nil {
		return nil, fmt.Errorf("list device streams: %w", err)
	}
	var keys []string
	for _, dp := range devPrefixes {
		dev := strings.TrimSuffix(strings.TrimPrefix(dp, h.eventlogPrefix()), "/")
		startAfter := fmt.Sprintf("%s%020d", dp, after.After(dev)+1)
		for {
			// HUB-10: retry list on throttling/transient S3 errors with backoff.
			var page []dssync.BlobInfo
			var next string
			if err := h.retry().do(ctx, func() error {
				var lerr error
				page, next, lerr = h.S3.ListObjectsV2(ctx, dp, startAfter, 1000)
				return lerr
			}); err != nil {
				return nil, fmt.Errorf("list events for device %s: %w", dev, err)
			}
			for _, obj := range page {
				keys = append(keys, obj.Key)
			}
			if next == "" {
				break
			}
			startAfter = next
		}
	}
	// Legacy dual-read: parse (device, seq) out of retired HLC-keyed objects
	// and apply the same per-device cursor at the KEY level, so only
	// not-yet-consumed legacy objects cost a GET. An unparseable key fails
	// open toward fetching — a parse bug must never silently lose events.
	// This freezes to a cheap empty-prefix LIST once a hub is migrated.
	legacyKeys, err := h.legacyEventKeys(ctx, after)
	if err != nil {
		return nil, err
	}
	keys = append(keys, legacyKeys...)
	// P5-HUB-04: fetch the selected objects with bounded concurrency instead
	// of one serial GetObject at a time, so a cold-start pull is not O(events)
	// serial round-trips. Results are placed by index so ordering is independent
	// of completion order; the final sort restores apply order.
	fetched := make([]state.Event, len(keys))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(r2PullConcurrency)
	for i, key := range keys {
		i, key := i, key
		g.Go(func() error {
			var raw []byte
			if err := h.retry().do(gctx, func() error {
				var gerr error
				raw, gerr = h.S3.GetObject(gctx, key)
				return gerr
			}); err != nil {
				return fmt.Errorf("get event object %s: %w", key, err)
			}
			var event state.Event
			if err := json.Unmarshal(raw, &event); err != nil {
				return fmt.Errorf("decode event object %s: %w", key, err)
			}
			fetched[i] = event
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	// Client-side filter on the event's OWN Seq (defense in depth for the
	// fail-open legacy GETs and any key/body mismatch) + event-ID dedup across
	// the two layouts (an event can exist under both after a future
	// migrate-events copy). Seq <= 0 events are always delivered (legacy
	// pre-sequence objects cannot be cursored; they dedup by ID).
	seen := make(map[string]bool, len(fetched))
	var out []state.Event
	for _, event := range fetched {
		if event.Seq > 0 && event.Seq <= after.After(event.DeviceID) {
			continue
		}
		if seen[event.ID] {
			continue
		}
		seen[event.ID] = true
		out = append(out, event)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].HLC == out[j].HLC {
			if out[i].DeviceID == out[j].DeviceID {
				return out[i].ID < out[j].ID
			}
			return out[i].DeviceID < out[j].DeviceID
		}
		return out[i].HLC < out[j].HLC
	})
	return out, nil
}

// legacyEventKeys lists the retired HLC-keyed layout and returns the keys
// whose parsed (device, seq) are beyond the per-device cursor — plus any key
// that does not parse (fail open toward the GET). Key shape:
//
//	workspaces/<ws>/events/<hlc pad20>/<device_id>/<seq>/<event_id>.json
func (h R2Hub) legacyEventKeys(ctx context.Context, after dssync.Cursor) ([]string, error) {
	prefix := h.legacyEventsPrefix()
	var keys []string
	startAfter := ""
	for {
		var page []dssync.BlobInfo
		var next string
		if err := h.retry().do(ctx, func() error {
			var lerr error
			page, next, lerr = h.S3.ListObjectsV2(ctx, prefix, startAfter, 1000)
			return lerr
		}); err != nil {
			return nil, fmt.Errorf("list legacy events: %w", err)
		}
		for _, obj := range page {
			rest := strings.TrimPrefix(obj.Key, prefix)
			parts := strings.Split(rest, "/")
			if len(parts) == 4 {
				if seq, perr := strconv.ParseInt(parts[2], 10, 64); perr == nil && seq > 0 {
					if seq <= after.After(parts[1]) {
						continue // already consumed; skip the GET
					}
				}
			}
			keys = append(keys, obj.Key)
		}
		if next == "" {
			break
		}
		startAfter = next
	}
	return keys, nil
}

// HasEvents reports whether this workspace has any events at all on the hub
// (P4-SEC-07 doctor mismatch check): one retried MaxKeys=1 ListObjectsV2 call
// per layout (seq-keyed, then legacy). It answers "is this prefix populated"
// cheaply, without paging the whole event log.
func (h R2Hub) HasEvents(ctx context.Context) (bool, error) {
	for _, prefix := range []string{h.eventlogPrefix(), h.legacyEventsPrefix()} {
		var page []dssync.BlobInfo
		if err := h.retry().do(ctx, func() error {
			var lerr error
			page, _, lerr = h.S3.ListObjectsV2(ctx, prefix, "", 1)
			return lerr
		}); err != nil {
			return false, fmt.Errorf("list events: %w", err)
		}
		if len(page) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// ListBlobs returns metadata for every blob in this workspace's blob prefix
// (P5-HUB-02), the enumeration primitive for mark-and-sweep hub GC.
func (h R2Hub) ListBlobs(ctx context.Context) ([]dssync.BlobInfo, error) {
	prefix := fmt.Sprintf("workspaces/%s/blobs/", h.WorkspaceID)
	var out []dssync.BlobInfo
	startAfter := ""
	for {
		var objs []dssync.BlobInfo
		var next string
		if err := h.retry().do(ctx, func() error {
			var lerr error
			objs, next, lerr = h.S3.ListObjectsV2(ctx, prefix, startAfter, 1000)
			return lerr
		}); err != nil {
			return nil, fmt.Errorf("list blobs: %w", err)
		}
		for _, obj := range objs {
			out = append(out, dssync.BlobInfo{Key: strings.TrimPrefix(obj.Key, prefix), LastModified: obj.LastModified})
		}
		if next == "" {
			break
		}
		startAfter = next
	}
	return out, nil
}

func (h R2Hub) PutBlob(ctx context.Context, sha256Hex string, r io.Reader) error {
	if !isValidHexKey(sha256Hex) {
		return dssync.ErrInvalidBlobKey
	}
	key := h.blobKey(sha256Hex)
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read blob: %w", err)
	}
	// HUB-09: rely on the conditional put alone; a separate ObjectExists/HEAD
	// only doubles request cost and reopens a TOCTOU window the conditional
	// put already closes. For a content-addressed blob a 412
	// PreconditionFailed is definitionally a dedup hit (same sha256 = same
	// ciphertext), so it is treated as idempotent success. HUB-10: retry
	// throttling/transient errors; 412 is terminal and handled as a dedup hit.
	if err := h.retry().do(ctx, func() error { return h.S3.PutObject(ctx, key, data, true) }); err != nil {
		if errors.Is(err, ErrPreconditionFailed) {
			return nil
		}
		return fmt.Errorf("put blob %s: %w", sha256Hex, err)
	}
	return nil
}

func (h R2Hub) GetBlob(ctx context.Context, sha256Hex string) (io.ReadCloser, error) {
	if !isValidHexKey(sha256Hex) {
		return nil, dssync.ErrInvalidBlobKey
	}
	// HUB-10: retry on throttling/transient errors; a missing blob
	// (ErrBlobNotFound) is terminal and returned immediately.
	var data []byte
	if err := h.retry().do(ctx, func() error {
		var gerr error
		data, gerr = h.S3.GetObject(ctx, h.blobKey(sha256Hex))
		return gerr
	}); err != nil {
		return nil, err
	}
	return io.NopCloser(bytesReader(data)), nil
}

// DeleteBlob removes a content-addressed blob from the hub (SEC-01/HUB-12). A
// missing blob is not an error (idempotent delete). On revoke, after a blob is
// rewrapped to the reduced recipient set and its references repointed, the old
// ciphertext is deleted so the revoked device can no longer fetch it.
func (h R2Hub) DeleteBlob(ctx context.Context, sha256Hex string) error {
	if !isValidHexKey(sha256Hex) {
		return dssync.ErrInvalidBlobKey
	}
	return h.retry().do(ctx, func() error { return h.S3.DeleteObject(ctx, h.blobKey(sha256Hex)) })
}

// isValidHexKey checks for a 64-char hex digest with no path separators.
func isValidHexKey(s string) bool {
	if len(s) != 64 || strings.ContainsAny(s, `/\`) {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

func bytesReader(data []byte) io.Reader {
	return &byteReader{data: data}
}

type byteReader struct {
	data []byte
	off  int
}

func (r *byteReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}

// ErrNotImplemented signals an S3Client method that has no production wiring yet.
var ErrNotImplemented = errors.New("s3 operation not implemented")

// ErrPreconditionFailed signals that a conditional PutObject (If-None-Match: *)
// was rejected because the object already exists (R2 error 10031 / HTTP 412).
// For content-addressed blobs and immutable event keys a 412 is definitionally
// an idempotent dedup hit, not an error (HUB-09): the same sha256 yields the
// same ciphertext, and a duplicate event key is the same event.
var ErrPreconditionFailed = errors.New("object already exists (conditional put failed)")

// ErrS3Throttle signals an R2/S3 throttling response (429 TooManyRequests / 503
// SlowDown) that is retryable after backoff (HUB-10).
var ErrS3Throttle = errors.New("s3 throttling (429/503 slow down)")

// ErrS3Transient signals an R2/S3 transient response (InternalError / connection
// reset) that is retryable after a short backoff (HUB-10).
var ErrS3Transient = errors.New("s3 transient (500/connection reset)")

// ErrS3Auth signals a credential/authorization failure (401/403,
// SignatureDoesNotMatch, InvalidAccessKeyId, AccessDenied). Terminal — never
// retried — and carries remediation guidance so a bad or unresolved secret
// (e.g. an op:// ref pasted where a literal was expected before P6-HUB-02)
// surfaces as an actionable message instead of a raw SDK error.
var ErrS3Auth = errors.New("s3 authentication/authorization failed")

// s3ErrorClass classifies an S3 operation error for retry purposes (HUB-10).
type s3ErrorClass int

const (
	s3Terminal  s3ErrorClass = iota // auth, precondition, not-found, malformed — fail fast
	s3Transient                     // 500 / connection reset — retry, short backoff
	s3Throttle                      // 429 / 503 SlowDown — retry, longer backoff
)

func classifyS3Error(err error) s3ErrorClass {
	switch {
	case errors.Is(err, ErrS3Throttle):
		return s3Throttle
	case errors.Is(err, ErrS3Transient):
		return s3Transient
	default:
		// ErrPreconditionFailed, ErrBlobNotFound, auth/malformed errors, and any
		// unclassified error are terminal — never retried.
		return s3Terminal
	}
}

// R2Retry configures R2Hub-level retry behavior (HUB-10): throttling and
// transient S3 errors are retried with capped exponential backoff plus full
// jitter; terminal errors fail fast. A real aws-sdk-go-v2 client wires its own
// standard retryer (retry.NewStandard + a token-bucket RateLimiter so retries
// cannot create a runaway billing loop); this seam is the R2Hub-level policy
// that works with any S3Client and is tested via fault injection.
type R2Retry struct {
	MaxAttempts   int           // total attempts including the first; 0 => default (3)
	BaseDelay     time.Duration // base delay for transient errors; 0 => 50ms
	ThrottleDelay time.Duration // base delay for throttling errors; 0 => 1s
	Cap           time.Duration // max backoff delay; 0 => 20s
	// Jitter returns a non-negative int64 in [0, n). Defaults to math/rand.Int63n.
	// Tests inject a deterministic source (e.g. always 0) for fast retries.
	Jitter func(n int64) int64
}

// policy returns r with defaults filled in for zero fields (HUB-10).
func (r R2Retry) policy() R2Retry {
	if r.MaxAttempts > 0 {
		return r
	}
	return R2Retry{
		MaxAttempts:   3,
		BaseDelay:     50 * time.Millisecond,
		ThrottleDelay: 1 * time.Second,
		Cap:           20 * time.Second,
	}
}

// do runs fn with retry, backoff, and full jitter (HUB-10). Throttling and
// transient errors are retried up to MaxAttempts; terminal errors fail fast on
// the first attempt. The context is honored between attempts so a stuck
// operation is bounded by ctx cancellation/deadline.
func (r R2Retry) do(ctx context.Context, fn func() error) error {
	p := r.policy()
	var lastErr error
	for attempt := 1; attempt <= p.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := fn()
		if err == nil {
			return nil
		}
		lastErr = err
		class := classifyS3Error(err)
		if class == s3Terminal || attempt == p.MaxAttempts {
			return err
		}
		if err := r.sleep(ctx, p, attempt, class); err != nil {
			return err
		}
	}
	return lastErr
}

// sleep backs off before a retry using capped exponential growth with full
// jitter (HUB-10): d = uniform[0, min(Cap, base*2^(attempt-1))]. Throttling
// uses a longer base than transient errors, matching AWS standard retry mode.
func (r R2Retry) sleep(ctx context.Context, p R2Retry, attempt int, class s3ErrorClass) error {
	base := p.BaseDelay
	if class == s3Throttle {
		base = p.ThrottleDelay
	}
	if base <= 0 {
		base = 50 * time.Millisecond
	}
	cap := p.Cap
	if cap <= 0 {
		cap = 20 * time.Second
	}
	exp := base * time.Duration(1<<(attempt-1))
	// QUAL-06: clamp overflow (attempt large enough that 2^(attempt-1)
	// overflows int64) to cap so jitter never receives a non-positive bound.
	if exp > cap || exp <= 0 {
		exp = cap
	}
	jitter := p.Jitter
	if jitter == nil {
		jitter = rand.Int63n
	}
	d := time.Duration(jitter(int64(exp) + 1))
	if d < 0 {
		d = 0
	}
	timer := time.NewTimer(d)
	select {
	case <-ctx.Done():
		timer.Stop()
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// R2Config configures the Cloudflare R2 backend credentials and endpoint
// (HUB-07). Two credential modes are supported:
//
//   - Self-hosted (single owner): a bucket-scoped R2 API token may be used
//     directly. The key is kept on the trusted device and never reaches runners.
//   - Hosted/SaaS: the parent R2 key stays only in trusted control-plane code.
//     Devices and runner Machines receive short-lived temporary credentials or
//     presigned URLs scoped to workspaces/<workspace_id>/... with the minimum
//     needed operations. Runner Machines never receive the parent key.
//
// PrefixScope, when set, restricts all operations to a workspace prefix so a
// scoped credential cannot touch another tenant's objects.
type R2Config struct {
	Endpoint    string // R2 S3 API endpoint (https://<account>.r2.cloudflarestorage.com)
	Bucket      string
	WorkspaceID string
	// CredentialMode is "self-hosted" (bucket-scoped key) or "hosted"
	// (temporary prefix-scoped credentials brokered by the control plane).
	CredentialMode string
	// PrefixScope restricts operations to workspaces/<workspace_id>/. Required
	// in hosted mode so a scoped key cannot access other tenants.
	PrefixScope string
}
