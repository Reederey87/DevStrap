package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
)

const (
	defaultDurabilityExportInterval = 24 * time.Hour
	durabilityExportMetaKey         = "durability_export_last_success"
	durabilityExportConfigKey       = "durability.export_interval"
	maxRetentionMirrorAttempts      = 3
)

type durabilityExportRecord struct {
	Replica        string    `json:"replica"`
	SnapshotSHA256 string    `json:"snapshot_sha256"`
	ExportedAt     time.Time `json:"exported_at"`
}

// durabilityExportInterval resolves the shared sync/run-loop schedule. Zero is
// an explicit disable; negative or malformed values are configuration errors.
func durabilityExportInterval(opts *options) (time.Duration, error) {
	raw := strings.TrimSpace(opts.v.GetString(durabilityExportConfigKey))
	if raw == "" {
		return defaultDurabilityExportInterval, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", durabilityExportConfigKey, raw, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("invalid %s %q: must be >= 0 (0 disables durability export)", durabilityExportConfigKey, raw)
	}
	return d, nil
}

func readDurabilityExportRecord(ctx context.Context, store *state.Store) (durabilityExportRecord, bool, error) {
	raw, ok, err := store.GetLocalMeta(ctx, durabilityExportMetaKey)
	if err != nil || !ok {
		return durabilityExportRecord{}, ok, err
	}
	var record durabilityExportRecord
	if err := json.Unmarshal([]byte(raw), &record); err != nil {
		return durabilityExportRecord{}, true, fmt.Errorf("parse last durability export: %w", err)
	}
	if record.Replica == "" || record.SnapshotSHA256 == "" || record.ExportedAt.IsZero() {
		return durabilityExportRecord{}, true, errors.New("parse last durability export: required field is empty")
	}
	return record, true, nil
}

func durabilityExportDue(ctx context.Context, store *state.Store, replica string, interval time.Duration, now time.Time) (bool, error) {
	record, ok, err := readDurabilityExportRecord(ctx, store)
	if err != nil {
		// A corrupt advisory timestamp must not permanently wedge backups. Export
		// now and replace it with a valid success record.
		return true, nil
	}
	if !ok || record.Replica != replica {
		return true, nil
	}
	age := now.Sub(record.ExportedAt)
	return age < 0 || age >= interval, nil
}

// maybeExportHubDurability mirrors the immutable snapshot named by the
// primary's signed retention head, then publishes that same head on the
// replica. Snapshot-before-manifest ordering means a direct bootstrap from the
// replica can never observe a head naming an object this export did not first
// write. No retention head means hub compact has never produced a snapshot;
// that is an informational skip, not a failed sync cycle.
func maybeExportHubDurability(ctx context.Context, stdout io.Writer, opts *options, store *state.Store, primary dssync.Hub, primaryHubFile string, now time.Time) error {
	replicaURI := strings.TrimSpace(opts.v.GetString("hub_replica"))
	if replicaURI == "" {
		return nil
	}
	interval, err := durabilityExportInterval(opts)
	if err != nil {
		return appError{code: exitInvalidConfig, err: err}
	}
	if interval == 0 {
		return nil
	}
	primaryURI := strings.TrimSpace(opts.v.GetString("hub"))
	if primaryHubFile != "" {
		primaryURI = "file:" + primaryHubFile
	}
	if primaryURI == replicaURI {
		return appError{code: exitInvalidConfig, err: errors.New("hub_replica must name a different backend from the primary hub")}
	}
	due, err := durabilityExportDue(ctx, store, replicaURI, interval, now)
	if err != nil || !due {
		return err
	}
	replica, _, err := replicaHubFromOptions(ctx, opts, store)
	if err != nil {
		return appError{code: exitInvalidConfig, err: fmt.Errorf("configure durability replica: %w", err)}
	}
	rawManifest, _, err := primary.GetRetention(ctx)
	if errors.Is(err, dssync.ErrRetentionNotFound) {
		opts.progressf(stdout, "Durability export skipped: primary hub has no compaction snapshot; run `devstrap hub compact` first.\n")
		return nil
	}
	if err != nil {
		return appError{code: exitNetwork, err: fmt.Errorf("read primary retention manifest for durability export: %w", err)}
	}
	manifest, err := dssync.ParseRetentionManifest(rawManifest)
	if err != nil {
		return appError{code: exitNetwork, err: fmt.Errorf("parse primary retention manifest for durability export: %w", err)}
	}
	workspaceID, err := store.WorkspaceID(ctx)
	if err != nil {
		return err
	}
	if manifest.WorkspaceID != workspaceID {
		return appError{code: exitInvalidConfig, err: fmt.Errorf("primary retention manifest workspace %s does not match local workspace %s", manifest.WorkspaceID, workspaceID)}
	}
	currentDevice, err := store.CurrentDevice(ctx)
	if err != nil {
		return err
	}
	producerKey := ""
	approved := false
	// "local" is the store's approved-self trust sentinel (remote peers use
	// "approved"). Require it explicitly: possession of a signing key alone
	// must not create a more-lenient producer path than remote recovery.
	if currentDeviceIsApprovedSnapshotProducer(currentDevice, manifest.ProducedBy) {
		producerKey, approved = currentDevice.SigningPublicKey, true
	} else {
		producerKey, approved, err = store.ApprovedDeviceSigningKey(ctx, manifest.ProducedBy)
		if err != nil {
			return err
		}
	}
	if !approved {
		return appError{code: exitInvalidConfig, err: fmt.Errorf("cannot export snapshot from unapproved producer %s; pin/approve the producer or publish a new snapshot from an approved device", manifest.ProducedBy)}
	}
	if err := dssync.VerifyRetentionManifest(manifest, producerKey); err != nil {
		return appError{code: exitInvalidConfig, err: fmt.Errorf("verify primary retention manifest for durability export: %w", err)}
	}
	if manifest.Snapshot.ProducedBy != manifest.ProducedBy {
		return appError{code: exitInvalidConfig, err: fmt.Errorf("primary retention manifest producer %s names snapshot producer %s", manifest.ProducedBy, manifest.Snapshot.ProducedBy)}
	}
	object, err := primary.GetSnapshotObject(ctx, manifest.Snapshot.SHA256)
	if err != nil {
		return appError{code: exitNetwork, err: fmt.Errorf("read primary snapshot %s for durability export: %w", manifest.Snapshot.SHA256, err)}
	}
	sum := sha256.Sum256(object)
	if got := hex.EncodeToString(sum[:]); got != manifest.Snapshot.SHA256 {
		return appError{code: exitNetwork, err: fmt.Errorf("primary snapshot hash mismatch during durability export: got %s, manifest names %s", got, manifest.Snapshot.SHA256)}
	}
	if err := replica.PutSnapshotObject(ctx, manifest.Snapshot.SHA256, object); err != nil {
		return appError{code: exitNetwork, err: fmt.Errorf("write durability replica snapshot: %w", err)}
	}
	if err := mirrorRetentionManifest(ctx, replica, rawManifest); err != nil {
		return appError{code: exitNetwork, err: fmt.Errorf("write durability replica retention manifest: %w", err)}
	}
	record := durabilityExportRecord{Replica: replicaURI, SnapshotSHA256: manifest.Snapshot.SHA256, ExportedAt: now.UTC()}
	rawRecord, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal durability export success: %w", err)
	}
	if err := store.SetLocalMeta(ctx, durabilityExportMetaKey, string(rawRecord)); err != nil {
		return fmt.Errorf("record durability export success: %w", err)
	}
	opts.progressf(stdout, "Exported hub snapshot %s to durability replica.\n", manifest.Snapshot.SHA256)
	return nil
}

func currentDeviceIsApprovedSnapshotProducer(device state.Device, producerID string) bool {
	return device.ID == producerID && device.TrustState == "local" && device.SigningPublicKey != ""
}

// mirrorRetentionManifest publishes raw after the referenced snapshot object.
// CAS retries tolerate another cooperating exporter racing this one; identical
// bytes or an already-more-advanced same-workspace head are success.
func mirrorRetentionManifest(ctx context.Context, replica dssync.Hub, raw []byte) error {
	for range maxRetentionMirrorAttempts {
		current, etag, err := replica.GetRetention(ctx)
		switch {
		case errors.Is(err, dssync.ErrRetentionNotFound):
			etag = ""
		case err != nil:
			return err
		case bytes.Equal(current, raw):
			return nil
		case len(current) > 0:
			alreadyAdvanced, err := reconcileRetentionAdvance(current, raw)
			if err != nil {
				return err
			}
			if alreadyAdvanced {
				return nil
			}
		}
		err = replica.PutRetention(ctx, raw, etag)
		if err == nil {
			return nil
		}
		if !errors.Is(err, dssync.ErrRetentionConflict) {
			return err
		}
	}
	return fmt.Errorf("%w: replica retention head changed during %d attempts", dssync.ErrRetentionConflict, maxRetentionMirrorAttempts)
}

// reconcileRetentionAdvance classifies two same-workspace heads by their HLC
// and every per-device floor. A current head that dominates next is the benign
// concurrent-exporter case: another device already published at least as much
// recovery state, so the attempted older write is a successful no-op. A next
// head that dominates current may replace it. Mixed/incomparable heads remain
// a hard conflict because neither is a safe monotonic successor of the other.
func reconcileRetentionAdvance(currentRaw, nextRaw []byte) (bool, error) {
	current, err := dssync.ParseRetentionManifest(currentRaw)
	if err != nil {
		return false, fmt.Errorf("parse existing replica retention manifest before overwrite: %w", err)
	}
	next, err := dssync.ParseRetentionManifest(nextRaw)
	if err != nil {
		return false, fmt.Errorf("parse next replica retention manifest: %w", err)
	}
	if current.WorkspaceID != next.WorkspaceID {
		return false, fmt.Errorf("refusing replica retention overwrite across workspaces %s -> %s", current.WorkspaceID, next.WorkspaceID)
	}
	currentDominates := current.ProducedAt >= next.ProducedAt
	for deviceID, floor := range next.Floors {
		if current.Floors[deviceID] < floor {
			currentDominates = false
			break
		}
	}
	if currentDominates {
		return true, nil
	}
	nextDominates := next.ProducedAt >= current.ProducedAt
	for deviceID, floor := range current.Floors {
		if next.Floors[deviceID] < floor {
			nextDominates = false
			break
		}
	}
	if nextDominates {
		return false, nil
	}
	return false, fmt.Errorf("refusing conflicting durability replica head: existing and primary retention progress are incomparable")
}

// exportHubDurabilityAfterSync preserves primary convergence when the optional
// replica is transiently unavailable. Invalid configuration remains a hard
// failure because retrying cannot repair it; operational read/write failures
// are loud but best-effort and doctor reports persistent export staleness.
func exportHubDurabilityAfterSync(ctx context.Context, stdout io.Writer, opts *options, store *state.Store, primary dssync.Hub, primaryHubFile string, now time.Time) error {
	err := maybeExportHubDurability(ctx, stdout, opts, store, primary, primaryHubFile, now)
	if err == nil {
		return nil
	}
	var configErr appError
	if errors.As(err, &configErr) && configErr.code == exitInvalidConfig {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "warning: hub durability export failed after primary sync; replication will retry: %v\n", err)
	return nil
}
