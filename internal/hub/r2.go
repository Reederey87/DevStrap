// R2 is S3-compatible with zero egress, strong consistency for object
// writes/listing, and conditional puts. All payloads and blobs are
// age-encrypted and Ed25519-signed before upload, so R2 stores only ciphertext
// plus a signed map — it can decrypt nothing and holds no private key.
//
// The event log is NOT one overwritten manifest object. Every event is an
// immutable, unique, lexicographically sortable object (HUB-06):
//
//	workspaces/<workspace_id>/events/<hlc-padded>/<device_id>/<seq>/<event_id>.json
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
	"sort"
	"strings"

	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
)

// S3Client is the minimal S3-compatible operation set the R2 backend needs
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
	// ListObjectsV2 returns object keys under prefix, lexicographically after
	// startAfter, up to maxKeys. When truncated, it returns the next key to
	// continue from.
	ListObjectsV2(ctx context.Context, prefix, startAfter string, maxKeys int) (keys []string, nextStartAfter string, err error)
}

// R2Hub is the Cloudflare R2 zero-knowledge Hub backend (HUB-02). It implements
// dssync.Hub. All content is client-side encrypted before it reaches S3Client.
type R2Hub struct {
	S3          S3Client
	WorkspaceID string
}

// Compile-time assertion that R2Hub satisfies dssync.Hub (HUB-01/HUB-02).
var _ dssync.Hub = R2Hub{}

// eventKey builds the immutable, lexicographically sortable object key for an
// event (HUB-06). The HLC is zero-padded to 20 digits so lexical ordering
// matches numeric ordering, and the device_id/seq/event_id suffix makes the key
// unique per event.
func (h R2Hub) eventKey(event state.Event) string {
	return fmt.Sprintf("workspaces/%s/events/%020d/%s/%d/%s.json",
		h.WorkspaceID, event.HLC, event.DeviceID, event.Seq, event.ID)
}

// blobKey builds the content-addressed object key for an encrypted blob
// (HUB-06).
func (h R2Hub) blobKey(sha256Hex string) string {
	return fmt.Sprintf("workspaces/%s/blobs/%s", h.WorkspaceID, sha256Hex)
}

// eventsPrefix is the ListObjectsV2 prefix for all events in this workspace.
func (h R2Hub) eventsPrefix() string {
	return fmt.Sprintf("workspaces/%s/events/", h.WorkspaceID)
}

func (h R2Hub) Push(ctx context.Context, events []state.Event) error {
	for _, event := range events {
		if ctx.Err() != nil {
			return ctx.Err()
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
		// 412 PreconditionFailed is a duplicate event and a no-op.
		if err := h.S3.PutObject(ctx, key, raw, true); err != nil {
			if errors.Is(err, ErrPreconditionFailed) {
				continue // idempotent dedup hit
			}
			return fmt.Errorf("put event %s: %w", event.ID, err)
		}
	}
	return nil
}

func (h R2Hub) Pull(ctx context.Context, afterHLC int64) ([]state.Event, error) {
	// HUB-06: pull with bounded ListObjectsV2 pages, start-after the
	// afterHLC-padded key so only newer events are listed. The cursor is the
	// HLC value; we encode it as the zero-padded prefix to start after.
	startAfter := fmt.Sprintf("%s%020d", h.eventsPrefix(), afterHLC)
	var out []state.Event
	for {
		keys, next, err := h.S3.ListObjectsV2(ctx, h.eventsPrefix(), startAfter, 1000)
		if err != nil {
			return nil, fmt.Errorf("list events: %w", err)
		}
		for _, key := range keys {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			raw, err := h.S3.GetObject(ctx, key)
			if err != nil {
				return nil, fmt.Errorf("get event object %s: %w", key, err)
			}
			var event state.Event
			if err := json.Unmarshal(raw, &event); err != nil {
				return nil, fmt.Errorf("decode event object %s: %w", key, err)
			}
			if event.HLC > afterHLC {
				out = append(out, event)
			}
		}
		if next == "" {
			break
		}
		startAfter = next
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
	// ciphertext), so it is treated as idempotent success.
	if err := h.S3.PutObject(ctx, key, data, true); err != nil {
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
	data, err := h.S3.GetObject(ctx, h.blobKey(sha256Hex))
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytesReader(data)), nil
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
