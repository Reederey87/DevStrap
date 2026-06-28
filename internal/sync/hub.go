package sync

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Reederey87/DevStrap/internal/state"
)

// ErrSnapshotRequired signals that the requested pull cursor has fallen behind
// the hub's retention horizon and the caller must perform a full-state snapshot
// exchange (import) before continuing with incremental pulls.
var ErrSnapshotRequired = errors.New("full snapshot required")

// ErrBlobNotFound signals that a requested content-addressed blob is not
// present on the hub. It wraps os.ErrNotExist so callers can test with
// errors.Is(err, os.ErrNotExist).
var ErrBlobNotFound = errors.New("blob not found")

// ErrInvalidBlobKey signals that a blob key (sha256 hex digest) is malformed.
var ErrInvalidBlobKey = errors.New("invalid blob key")

// Hub is the two-plane zero-knowledge sync backend (HUB-01): (a) the signed
// HLC-ordered namespace-map event log and (b) the content-addressed encrypted
// blob store. The hub sees only ciphertext plus a signed map. Implementations
// must be safe for concurrent use.
//
// Event plane:
//   - Push appends locally-originated events. Duplicate event IDs are ignored
//     (idempotent), so re-pushing already-delivered events is safe.
//   - Pull returns events with HLC strictly greater than afterHLC in
//     deterministic order (HLC, device_id, id). If afterHLC falls below the
//     retention horizon, Pull returns ErrSnapshotRequired so the caller performs
//     a full-state snapshot exchange before resuming incremental pulls.
//
// Blob plane:
//   - PutBlob stores a content-addressed encrypted blob keyed by its sha256 hex
//     digest. Writes are idempotent: a blob already present is a no-op.
//   - GetBlob returns the blob as a stream the caller must close. A missing
//     blob returns an error wrapping os.ErrNotExist.
//
// The object-key contract is immutable: events and blobs are addressed by
// content-derived, collision-resistant identifiers and are never overwritten in
// place (HUB-06).
type Hub interface {
	Push(ctx context.Context, events []state.Event) error
	Pull(ctx context.Context, afterHLC int64) ([]state.Event, error)
	PutBlob(ctx context.Context, sha256Hex string, r io.Reader) error
	GetBlob(ctx context.Context, sha256Hex string) (io.ReadCloser, error)
}

// FileHub is a file-backed test Hub (HUB-01). The event log is a single JSON
// array file; blobs are stored in a sibling directory keyed by sha256 hex. It
// is retained ONLY for tests and the --hub-file spike; the production backend
// is the R2/S3 implementation (HUB-02).
type FileHub struct {
	Path         string
	RetentionHLC int64
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

func (h FileHub) Pull(ctx context.Context, afterHLC int64) ([]state.Event, error) {
	if h.RetentionHLC > 0 && afterHLC < h.RetentionHLC {
		return nil, ErrSnapshotRequired
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
		if event.HLC > afterHLC {
			out = append(out, event)
		}
	}
	sortEvents(out)
	return out, nil
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

func (h FileHub) blobDir() string {
	base := strings.TrimSuffix(filepath.Base(h.Path), filepath.Ext(h.Path))
	return filepath.Join(filepath.Dir(h.Path), base+"-blobs")
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
