package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Reederey87/DevStrap/internal/state"
)

// ErrSnapshotRequired signals that the requested pull cursor has fallen behind
// the hub's retention horizon and the caller must perform a full-state snapshot
// exchange (import) before continuing with incremental pulls.
var ErrSnapshotRequired = errors.New("full snapshot required")

// Cursor is the sync transport cursor (P5-SYNC-01): for each origin device on
// the hub, the highest per-device sequence number such that every event with
// Seq <= it has been pulled AND consumed (applied, deduped, or permanently
// quarantined). It is deliberately decoupled from the HLC, which remains the
// apply-ordering key only: every device's own event stream is gapless in Seq
// (UNIQUE events(device_id, seq), assigned in the same transaction as the HLC),
// so a per-device Seq cursor can never skip an event no matter how late it
// lands on the hub — the "offline device forgot to push, syncs late" scenario
// an HLC watermark permanently stranded. A device absent from the map has
// cursor 0 (pull from the beginning).
type Cursor map[string]int64

// After returns the cursor position for one origin device (0 when unknown).
func (c Cursor) After(deviceID string) int64 {
	if c == nil {
		return 0
	}
	return c[deviceID]
}

// ErrBlobNotFound signals that a requested content-addressed blob is not
// present on the hub. It wraps os.ErrNotExist so callers can test with
// errors.Is(err, os.ErrNotExist).
var ErrBlobNotFound = errors.New("blob not found")

// ErrInvalidBlobKey signals that a blob key (sha256 hex digest) is malformed.
var ErrInvalidBlobKey = errors.New("invalid blob key")

// BlobInfo describes one blob on the hub: its sha256 hex key and the
// hub-reported creation/modification time (zero when the backend cannot
// provide one). P6-HUB-01: GC uses LastModified for an age grace window.
type BlobInfo struct {
	Key          string
	LastModified time.Time
}

// Hub is the two-plane zero-knowledge sync backend (HUB-01): (a) the signed
// HLC-ordered namespace-map event log and (b) the content-addressed encrypted
// blob store. The hub sees only ciphertext plus a signed carrier map. When
// wrapped by EncryptedHub (P4-SEC-02/SEC-07), event-log payloads are
// envelope-encrypted (XChaCha20-Poly1305 under a per-epoch Workspace Content
// Key) so the hub never stores plaintext Type/PayloadJSON/ContentHash; the
// carrier (ID/DeviceID/Seq/HLC/DeviceSig) remains plaintext so hub ordering,
// dedup, and Ed25519 signature verification are unchanged. Implementations
// must be safe for concurrent use.
//
// Event plane:
//   - Push appends locally-originated events. Duplicate event IDs are ignored
//     (idempotent), so re-pushing already-delivered events is safe.
//   - Pull returns, for EVERY origin device present on the hub (device
//     discovery is the hub's job, so a brand-new device's stream is picked up
//     with no cursor entry), every event with Seq > after[DeviceID], in
//     deterministic apply order (HLC, device_id, id). The per-device Seq
//     boundary is exact — Seq is unique per device — so the old inclusive-HLC
//     boundary re-delivery (HUB-13) is retired; no boundary overlap exists.
//     Events with Seq <= 0 (pre-sequence legacy objects) are always returned:
//     they cannot be cursored and rely on event-ID dedup. If any device's
//     cursor has fallen behind the hub's retention horizon (after[dev]+1 <
//     minRetainedSeq[dev]), Pull returns ErrSnapshotRequired so the caller
//     performs a full-state snapshot exchange before resuming incremental
//     pulls.
//
// Blob plane:
//   - PutBlob stores a content-addressed encrypted blob keyed by its sha256 hex
//     digest. Writes are idempotent: a blob already present is a no-op.
//   - GetBlob returns the blob as a stream the caller must close. A missing
//     blob returns an error wrapping os.ErrNotExist.
//   - DeleteBlob removes a content-addressed blob. It is the reclamation
//     primitive that makes blob/event GC possible (HUB-12) and lets device
//     revoke delete superseded ciphertext so a revoked key can no longer fetch
//     it (SEC-01). A missing blob is not an error (idempotent delete).
//
// The object-key contract is immutable: events and blobs are addressed by
// content-derived, collision-resistant identifiers and are never overwritten in
// place (HUB-06). The single exception is the retention manifest, which is a
// mutable head object guarded by compare-and-swap (PutRetention).
//
// Retention/snapshot plane (P4-SYNC-02 / P4-HUB-11 / P6-HUB-04):
//   - GetRetention returns the raw signed retention-manifest bytes plus an
//     opaque etag for CAS. Absent manifest (no compaction ever) returns
//     ErrRetentionNotFound.
//   - PutRetention writes the manifest conditionally: ifMatchETag "" means
//     create-only (the manifest must not exist yet); otherwise the write
//     succeeds only if the current object still matches the etag. A lost race
//     returns ErrRetentionConflict. The hub cannot verify the signature — the
//     manifest is verified fail-closed by importers (see snapshot.go).
//   - Snapshot objects are content-addressed by the sha256 of their sealed
//     bytes (concurrent compactors can never clobber each other) and immutable;
//     DeleteSnapshotObject exists only so compaction can prune superseded
//     snapshots.
//   - CompactEventsBelow deletes event objects strictly below each device's
//     floor (Seq < floors[dev]). Callers must have durably published a
//     superseding snapshot + manifest FIRST — the hub does not enforce the
//     ordering; the compactor's confirm-before-delete protocol does.
type Hub interface {
	Push(ctx context.Context, events []state.Event) error
	Pull(ctx context.Context, after Cursor) ([]state.Event, error)
	PutBlob(ctx context.Context, sha256Hex string, r io.Reader) error
	GetBlob(ctx context.Context, sha256Hex string) (io.ReadCloser, error)
	DeleteBlob(ctx context.Context, sha256Hex string) error
	// ListBlobs returns metadata for every blob currently on the hub
	// (P5-HUB-02). It is the enumeration primitive for mark-and-sweep hub GC:
	// list everything, delete what no current binding/snapshot references.
	ListBlobs(ctx context.Context) ([]BlobInfo, error)
	GetRetention(ctx context.Context) (raw []byte, etag string, err error)
	PutRetention(ctx context.Context, raw []byte, ifMatchETag string) error
	PutSnapshotObject(ctx context.Context, sha256Hex string, body []byte) error
	GetSnapshotObject(ctx context.Context, sha256Hex string) ([]byte, error)
	ListSnapshotObjects(ctx context.Context) ([]BlobInfo, error)
	DeleteSnapshotObject(ctx context.Context, sha256Hex string) error
	CompactEventsBelow(ctx context.Context, floors Cursor) (deleted int, err error)
}

// FileHub is a file-backed test Hub (HUB-01). The event log is a single JSON
// array file; blobs are stored in a sibling directory keyed by sha256 hex. It
// is retained ONLY for tests and the --hub-file spike; the production backend
// is the R2/S3 implementation (HUB-02).
type FileHub struct {
	Path string
	// RetentionSeqs is the hub's per-device retention horizon (P5-HUB-03,
	// re-based on the Seq transport cursor): for each origin device, the
	// minimum Seq still retained. A Pull whose cursor would leave a gap below
	// a device's floor (after[dev]+1 < RetentionSeqs[dev]) returns
	// ErrSnapshotRequired. Empty means "no compaction yet" (everything
	// retained). Test-only plumbing until snapshot exchange lands
	// (P4-SYNC-02/P4-HUB-11).
	RetentionSeqs map[string]int64
}

func (h FileHub) Push(ctx context.Context, events []state.Event) error {
	all, err := h.read()
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, event := range all {
		seen[event.ID] = true
	}
	for _, event := range events {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !seen[event.ID] {
			all = append(all, event)
			seen[event.ID] = true
		}
	}
	sortEvents(all)
	return h.write(all)
}

func (h FileHub) Pull(ctx context.Context, after Cursor) ([]state.Event, error) {
	// P5-HUB-03 (Seq-re-based): a cursor below any device's retention floor
	// means the incremental log has a gap only a snapshot can fill. The floor
	// comes from the hub's retention manifest (P6-HUB-04), merged with the
	// RetentionSeqs test override. The manifest is read UNVERIFIED here — the
	// backend holds no device registry; an unverified floor can only FORCE the
	// snapshot path, where fail-closed verification lives.
	floors, err := h.retentionFloors(ctx)
	if err != nil {
		return nil, err
	}
	for dev, minRetained := range floors {
		if minRetained > 0 && after.After(dev)+1 < minRetained {
			return nil, ErrSnapshotRequired
		}
	}
	all, err := h.read()
	if err != nil {
		return nil, err
	}
	var out []state.Event
	for _, event := range all {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Seq <= 0 (pre-sequence legacy) cannot be cursored: always deliver,
		// rely on event-ID dedup. Otherwise the per-device Seq boundary is
		// exact (Seq is unique per device) — no HUB-13 overlap re-delivery.
		if event.Seq <= 0 || event.Seq > after.After(event.DeviceID) {
			out = append(out, event)
		}
	}
	sortEvents(out)
	return out, nil
}

// HasEvents reports whether any event has ever been recorded on this hub
// (P4-SEC-07 doctor mismatch check).
func (h FileHub) HasEvents(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	all, err := h.read()
	if err != nil {
		return false, err
	}
	return len(all) > 0, nil
}

// PutBlob stores an encrypted blob keyed by its sha256 hex digest. The blob is
// content-addressed: writing the same digest twice is a no-op.
func (h FileHub) PutBlob(ctx context.Context, sha256Hex string, r io.Reader) error {
	if err := validateBlobKey(sha256Hex); err != nil {
		return err
	}
	if err := os.MkdirAll(h.blobDir(), 0o700); err != nil {
		return fmt.Errorf("create blob dir: %w", err)
	}
	dst := h.blobPath(sha256Hex)
	if _, err := os.Stat(dst); err == nil {
		return nil // idempotent: blob already present
	}
	tmp, err := os.CreateTemp(h.blobDir(), ".blob-*.tmp")
	if err != nil {
		return fmt.Errorf("create blob temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := io.Copy(tmp, r); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close blob temp file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("secure blob: %w", err)
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return fmt.Errorf("install blob: %w", err)
	}
	cleanup = false
	return nil
}

// GetBlob returns an encrypted blob as a stream. The caller must close the
// reader. A missing blob returns an error wrapping os.ErrNotExist.
func (h FileHub) GetBlob(_ context.Context, sha256Hex string) (io.ReadCloser, error) {
	if err := validateBlobKey(sha256Hex); err != nil {
		return nil, err
	}
	//nolint:gosec // The path is derived from a validated hex digest under the hub blob dir.
	f, err := os.Open(h.blobPath(sha256Hex))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrBlobNotFound, sha256Hex)
		}
		return nil, fmt.Errorf("open blob: %w", err)
	}
	return f, nil
}

// DeleteBlob removes a content-addressed blob (SEC-01/HUB-12). A missing blob is
// not an error (idempotent delete), so revoke/GC can call it unconditionally for
// superseded ciphertext.
func (h FileHub) DeleteBlob(_ context.Context, sha256Hex string) error {
	if err := validateBlobKey(sha256Hex); err != nil {
		return err
	}
	err := os.Remove(h.blobPath(sha256Hex))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete blob: %w", err)
	}
	return nil
}

// ListBlobs returns metadata for every blob in the hub's blob directory
// (P5-HUB-02).
func (h FileHub) ListBlobs(_ context.Context) ([]BlobInfo, error) {
	entries, err := os.ReadDir(h.blobDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list hub blobs: %w", err)
	}
	var out []BlobInfo
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".blob") {
			continue
		}
		key := strings.TrimSuffix(name, ".blob")
		if validateBlobKey(key) == nil {
			info, err := e.Info()
			if err != nil {
				return nil, fmt.Errorf("stat hub blob %s: %w", key, err)
			}
			out = append(out, BlobInfo{Key: key, LastModified: info.ModTime()})
		}
	}
	return out, nil
}

// retentionFloors merges the retention manifest's per-device floors (when a
// manifest exists) with the RetentionSeqs test override; the override wins
// per device so tests can tighten a floor without writing a manifest.
func (h FileHub) retentionFloors(ctx context.Context) (map[string]int64, error) {
	floors := map[string]int64{}
	raw, _, err := h.GetRetention(ctx)
	switch {
	case errors.Is(err, ErrRetentionNotFound):
		// no compaction yet
	case err != nil:
		return nil, err
	default:
		parsed, perr := ParseRetentionFloors(raw)
		if perr != nil {
			// Fail closed: a garbled marker must not read as "no floor".
			return nil, fmt.Errorf("read retention manifest: %w", perr)
		}
		for dev, seq := range parsed {
			floors[dev] = seq
		}
	}
	for dev, seq := range h.RetentionSeqs {
		// The test override may only TIGHTEN a floor (raise it) — an override
		// below the manifest floor would let a cursor pull incrementally across
		// a compacted gap (CodeRabbit, PR #65).
		if seq > floors[dev] {
			floors[dev] = seq
		}
	}
	return floors, nil
}

// GetRetention returns the raw retention-manifest bytes plus an etag (the
// sha256 hex of the bytes — FileHub has no HTTP etags).
func (h FileHub) GetRetention(_ context.Context) ([]byte, string, error) {
	raw, err := os.ReadFile(h.retentionPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", ErrRetentionNotFound
		}
		return nil, "", fmt.Errorf("read retention manifest: %w", err)
	}
	return raw, contentETag(raw), nil
}

// PutRetention writes the retention manifest with compare-and-swap semantics:
// ifMatchETag "" requires that no manifest exists yet; otherwise the current
// bytes must still hash to ifMatchETag. A lost race is ErrRetentionConflict.
// The read-check-write section is serialized by an O_EXCL lock file so two
// concurrent writers — in one process or across `--hub-file` processes — can
// never both pass the etag check and silently overwrite each other
// (post-#65 Codex review, P2).
func (h FileHub) PutRetention(_ context.Context, raw []byte, ifMatchETag string) error {
	if err := os.MkdirAll(h.metaDir(), 0o700); err != nil {
		return fmt.Errorf("create hub meta dir: %w", err)
	}
	unlock, err := acquireLockFile(h.retentionPath() + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	current, err := os.ReadFile(h.retentionPath())
	switch {
	case os.IsNotExist(err):
		if ifMatchETag != "" {
			return fmt.Errorf("%w: manifest absent, expected etag %s", ErrRetentionConflict, ifMatchETag)
		}
	case err != nil:
		return fmt.Errorf("read retention manifest: %w", err)
	default:
		if ifMatchETag == "" {
			return fmt.Errorf("%w: manifest already exists", ErrRetentionConflict)
		}
		if contentETag(current) != ifMatchETag {
			return fmt.Errorf("%w: etag mismatch", ErrRetentionConflict)
		}
	}
	tmp, err := os.CreateTemp(h.metaDir(), ".retention-*.tmp")
	if err != nil {
		return fmt.Errorf("create retention temp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write retention temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close retention temp: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("secure retention temp: %w", err)
	}
	if err := os.Rename(tmpPath, h.retentionPath()); err != nil {
		return fmt.Errorf("install retention manifest: %w", err)
	}
	cleanup = false
	return nil
}

// acquireLockFile takes an O_CREATE|O_EXCL lock file, retrying briefly and
// breaking locks older than 10s (a crashed process must not wedge the hub
// file forever). Returns the release func.
func acquireLockFile(path string) (func(), error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // lock file under the hub meta dir
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("acquire hub lock %s: %w", path, err)
		}
		if info, serr := os.Stat(path); serr == nil && time.Since(info.ModTime()) > 10*time.Second {
			_ = os.Remove(path) // stale lock from a crashed holder
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("acquire hub lock %s: timed out (held by another process?)", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// PutSnapshotObject stores a sealed snapshot object keyed by the sha256 of its
// bytes. Content-addressed: an existing object is a no-op.
func (h FileHub) PutSnapshotObject(_ context.Context, sha256Hex string, body []byte) error {
	if err := validateBlobKey(sha256Hex); err != nil {
		return err
	}
	if contentETag(body) != sha256Hex {
		return fmt.Errorf("%w: snapshot body does not hash to its key %s", ErrInvalidBlobKey, sha256Hex)
	}
	if err := os.MkdirAll(h.snapshotDir(), 0o700); err != nil {
		return fmt.Errorf("create hub snapshot dir: %w", err)
	}
	dst := h.snapshotPath(sha256Hex)
	if _, err := os.Stat(dst); err == nil {
		return nil // content-addressed dedup
	}
	if err := os.WriteFile(dst, body, 0o600); err != nil {
		return fmt.Errorf("write snapshot object: %w", err)
	}
	return nil
}

// GetSnapshotObject returns a sealed snapshot object. Missing objects wrap
// ErrBlobNotFound.
func (h FileHub) GetSnapshotObject(_ context.Context, sha256Hex string) ([]byte, error) {
	if err := validateBlobKey(sha256Hex); err != nil {
		return nil, err
	}
	//nolint:gosec // The path is derived from a validated hex digest under the hub snapshot dir.
	raw, err := os.ReadFile(h.snapshotPath(sha256Hex))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: snapshot %s", ErrBlobNotFound, sha256Hex)
		}
		return nil, fmt.Errorf("read snapshot object: %w", err)
	}
	return raw, nil
}

// ListSnapshotObjects returns metadata for every snapshot object on the hub
// (compaction prunes superseded ones by age, keeping the newest N).
func (h FileHub) ListSnapshotObjects(_ context.Context) ([]BlobInfo, error) {
	entries, err := os.ReadDir(h.snapshotDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list hub snapshots: %w", err)
	}
	var out []BlobInfo
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		key := strings.TrimSuffix(e.Name(), ".json")
		if validateBlobKey(key) == nil {
			info, err := e.Info()
			if err != nil {
				return nil, fmt.Errorf("stat hub snapshot %s: %w", key, err)
			}
			out = append(out, BlobInfo{Key: key, LastModified: info.ModTime()})
		}
	}
	return out, nil
}

// DeleteSnapshotObject removes a superseded snapshot object (idempotent).
func (h FileHub) DeleteSnapshotObject(_ context.Context, sha256Hex string) error {
	if err := validateBlobKey(sha256Hex); err != nil {
		return err
	}
	if err := os.Remove(h.snapshotPath(sha256Hex)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete snapshot object: %w", err)
	}
	return nil
}

// CompactEventsBelow deletes events strictly below each device's floor
// (Seq > 0 && Seq < floors[dev]). Pre-sequence events (Seq <= 0) are never
// compacted — they cannot be covered by a Seq floor. The caller must have
// durably published the superseding snapshot + manifest first.
func (h FileHub) CompactEventsBelow(ctx context.Context, floors Cursor) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	all, err := h.read()
	if err != nil {
		return 0, err
	}
	kept := all[:0]
	deleted := 0
	for _, event := range all {
		if event.Seq > 0 && floors.After(event.DeviceID) > 0 && event.Seq < floors.After(event.DeviceID) {
			deleted++
			continue
		}
		kept = append(kept, event)
	}
	if deleted == 0 {
		return 0, nil
	}
	if err := h.write(kept); err != nil {
		return 0, err
	}
	return deleted, nil
}

func (h FileHub) baseName() string {
	return strings.TrimSuffix(filepath.Base(h.Path), filepath.Ext(h.Path))
}

func (h FileHub) metaDir() string {
	return filepath.Join(filepath.Dir(h.Path), h.baseName()+"-meta")
}

func (h FileHub) snapshotDir() string {
	return filepath.Join(filepath.Dir(h.Path), h.baseName()+"-snapshots")
}

func (h FileHub) retentionPath() string {
	return filepath.Join(h.metaDir(), "retention.json")
}

func (h FileHub) snapshotPath(sha256Hex string) string {
	return filepath.Join(h.snapshotDir(), sha256Hex+".json")
}

// contentETag is FileHub's etag: the sha256 hex of the object bytes.
func contentETag(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (h FileHub) blobDir() string {
	return filepath.Join(filepath.Dir(h.Path), h.baseName()+"-blobs")
}

func (h FileHub) blobPath(sha256Hex string) string {
	return filepath.Join(h.blobDir(), sha256Hex+".blob")
}

func (h FileHub) read() ([]state.Event, error) {
	raw, err := os.ReadFile(h.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read hub: %w", err)
	}
	var events []state.Event
	if len(raw) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &events); err != nil {
		return nil, fmt.Errorf("decode hub: %w", err)
	}
	return events, nil
}

func (h FileHub) write(events []state.Event) error {
	if err := os.MkdirAll(filepath.Dir(h.Path), 0o700); err != nil {
		return fmt.Errorf("create hub dir: %w", err)
	}
	raw, err := json.MarshalIndent(events, "", "  ")
	if err != nil {
		return fmt.Errorf("encode hub: %w", err)
	}
	if err := os.WriteFile(h.Path, raw, 0o600); err != nil {
		return fmt.Errorf("write hub: %w", err)
	}
	return nil
}

func sortEvents(events []state.Event) {
	sort.Slice(events, func(i, j int) bool {
		if events[i].HLC == events[j].HLC {
			if events[i].DeviceID == events[j].DeviceID {
				return events[i].ID < events[j].ID
			}
			return events[i].DeviceID < events[j].DeviceID
		}
		return events[i].HLC < events[j].HLC
	})
}

// validateBlobKey ensures a blob key is a lowercase or uppercase hex sha256
// digest (64 chars) with no path separators, so it cannot escape the blob dir.
func validateBlobKey(sha256Hex string) error {
	if len(sha256Hex) != hex.EncodedLen(32) {
		return fmt.Errorf("%w: expected 64 hex chars, got %d", ErrInvalidBlobKey, len(sha256Hex))
	}
	if strings.ContainsAny(sha256Hex, `/\`) {
		return fmt.Errorf("%w: contains path separator", ErrInvalidBlobKey)
	}
	if _, err := hex.DecodeString(sha256Hex); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidBlobKey, err)
	}
	return nil
}

// Compile-time assertion that FileHub satisfies Hub (HUB-01).
var _ Hub = FileHub{}
