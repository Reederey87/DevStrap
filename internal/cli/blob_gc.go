package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Reederey87/DevStrap/internal/config"
	"github.com/Reederey87/DevStrap/internal/envbundle"
	"github.com/Reederey87/DevStrap/internal/logging"
	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
)

// rewrapBlobsOnRevoke re-encrypts every referenced blob to the current approved
// recipient set (which no longer includes the revoked device) and repoints the
// references (HUB-04). age has no native revocation, so a blob already
// encrypted to a revoked device's X25519 recipient stays decryptable by that key
// forever — re-encryption to the reduced recipient set limits future exposure,
// and the underlying secrets are already flagged needs_rotation by the revoke
// command.
//
// SEC-01: when a hub is provided, revocation is a hub operation — blobs not
// cached locally are pulled from the hub first (today they were skipped, leaving
// them encrypted to the revoked key), the rewrapped blob is pushed to the hub,
// and the old ciphertext is deleted from the hub so the revoked device can no
// longer fetch it. The old blob is only deleted once no binding/snapshot still
// references it (UpdateBlobRef bulk-repoints, so the old ref is absent). When no
// hub is provided (--hub-file not given to revoke/lost), rewrap is local-only
// and hub-side cleanup is deferred to the next sync.
func rewrapBlobsOnRevoke(ctx context.Context, store *state.Store, opts *options, hub dssync.Hub) (int, error) {
	recipients, err := store.ApprovedRecipients(ctx)
	if err != nil {
		return 0, err
	}
	device, err := store.CurrentDevice(ctx)
	if err != nil {
		return 0, err
	}
	keyStore, err := resolveKeyStore(ctx, opts.paths(), store)
	if err != nil {
		return 0, err
	}
	identity, err := keyStore.Read(ctx, device.ID)
	if err != nil {
		return 0, fmt.Errorf("read local device identity: %w", err)
	}
	rewrapped := 0

	// P5-SEC-04: env secret blobs are local-only — they are never pushed to or
	// synced through the hub (synced encrypted env-bundle exchange is not built).
	// Rewrap them in place; never PutBlob/DeleteBlob them on the hub, and never
	// orphan them with an event.
	envRefs, err := store.EnvBlobRefs(ctx)
	if err != nil {
		return rewrapped, err
	}
	for _, ref := range envRefs {
		if ok, ferr := rewrapLocalEnvBlob(ctx, store, opts, identity.Private, recipients, ref); ferr != nil {
			return rewrapped, ferr
		} else if ok {
			rewrapped++
		}
	}

	// P5-SEC-01: draft blobs ARE synced through the hub and are reconstructed on
	// every device from the immutable draft.snapshot.created event payload. So a
	// rewrap MUST emit a superseding event carrying the new ref before the old
	// hub ciphertext is deleted — otherwise peers replay the old (deleted) ref
	// and permanently lose draft access.
	draftRefs, err := store.DraftBlobRefs(ctx)
	if err != nil {
		return rewrapped, err
	}
	for _, ref := range draftRefs {
		ok, ferr := rewrapDraftBlob(ctx, store, opts, hub, identity.Private, recipients, ref)
		if ferr != nil {
			return rewrapped, ferr
		}
		if ok {
			rewrapped++
		}
	}
	return rewrapped, nil
}

// rewrapLocalEnvBlob re-encrypts a local-only env blob to the reduced recipient
// set in place (P5-SEC-04). It never touches the hub. Returns (rewrapped, err).
func rewrapLocalEnvBlob(ctx context.Context, store *state.Store, opts *options, identity string, recipients []string, ref string) (bool, error) {
	ciphertext, err := readEnvBlob(opts.paths(), ref)
	if err != nil {
		logging.Logger(ctx).Warn("rewrap: env blob not cached locally, skipping", "ref", ref, "err", err.Error())
		return false, nil
	}
	newCiphertext, newRef, err := envbundle.Rewrap(ciphertext, identity, recipients)
	if err != nil {
		logging.Logger(ctx).Warn("rewrap: env decrypt/re-encrypt failed, skipping", "ref", ref, "err", err.Error())
		return false, nil
	}
	if err := writeEnvBlob(opts.paths(), newRef, newCiphertext); err != nil {
		return false, fmt.Errorf("write rewrapped env blob: %w", err)
	}
	if err := store.UpdateBlobRef(ctx, ref, newRef); err != nil {
		return false, fmt.Errorf("update env blob ref: %w", err)
	}
	return true, nil
}

// rewrapDraftBlob re-encrypts a draft blob and makes the new ref discoverable
// before the old ciphertext is reclaimed (P5-SEC-01). It emits a superseding
// draft.snapshot.created event per affected project (so peers reconstruct the
// new ref), repoints local references, and then either deletes the old hub
// ciphertext immediately (hub present, after the event + new blob are durably
// pushed) or queues it for deletion on the next hub-enabled sync (P5-PROD-02).
func rewrapDraftBlob(ctx context.Context, store *state.Store, opts *options, hub dssync.Hub, identity string, recipients []string, ref string) (bool, error) {
	// Look up affected snapshots BEFORE repointing so we still resolve by oldRef.
	snaps, err := store.DraftSnapshotsForBlobRef(ctx, ref)
	if err != nil {
		return false, err
	}
	ciphertext, err := readEnvBlob(opts.paths(), ref)
	if err != nil {
		if hub == nil {
			logging.Logger(ctx).Warn("rewrap: draft blob not cached locally and no hub provided, skipping", "ref", ref)
			return false, nil
		}
		fetched, ferr := fetchBlobForRewrap(ctx, hub, ref)
		if ferr != nil {
			logging.Logger(ctx).Warn("rewrap: could not fetch draft blob from hub, skipping", "ref", ref, "err", ferr.Error())
			return false, nil
		}
		ciphertext = fetched
	}
	newCiphertext, newRef, err := envbundle.Rewrap(ciphertext, identity, recipients)
	if err != nil {
		logging.Logger(ctx).Warn("rewrap: draft decrypt/re-encrypt failed, skipping", "ref", ref, "err", err.Error())
		return false, nil
	}
	if err := writeEnvBlob(opts.paths(), newRef, newCiphertext); err != nil {
		return false, fmt.Errorf("write rewrapped draft blob: %w", err)
	}
	// P5-SEC-01: emit a superseding event per affected project FIRST so the new
	// ref is in the local event log (pushed on the next sync) and dominates the
	// old snapshot's HLC, then repoint local references.
	var events []state.Event
	for _, snap := range snaps {
		ev, eerr := emitSupersedingDraftSnapshot(ctx, store, snap.NamespaceID, snap.Path, newRef, snap.ByteSize, snap.FileCount)
		if eerr != nil {
			return false, fmt.Errorf("emit superseding draft snapshot for %s: %w", snap.Path, eerr)
		}
		events = append(events, ev)
	}
	if err := store.UpdateBlobRef(ctx, ref, newRef); err != nil {
		return false, fmt.Errorf("update draft blob ref: %w", err)
	}
	if hub != nil {
		rewrapHubCleanup(ctx, hub, store, ref, newRef, newCiphertext, events)
	} else {
		// P5-PROD-02: no hub now — queue the orphaned old ref so the next
		// hub-enabled sync deletes it (after it has pushed the superseding event
		// and new blob), instead of leaving it stranded with a misleading note.
		if qerr := store.QueuePendingHubDelete(ctx, ref); qerr != nil {
			logging.Logger(ctx).Warn("rewrap: failed to queue old ref for hub deletion", "ref", ref, "err", qerr.Error())
		}
	}
	return true, nil
}

// emitSupersedingDraftSnapshot emits a fresh draft.snapshot.created event
// carrying newRef so peers reconstruct it instead of a deleted old ref
// (P5-SEC-01). Returns the stamped event so the caller can push it.
// After the caller's UpdateBlobRef repoint, the origin intentionally holds two
// rows for newRef (the repointed original and this superseding row); the
// duplicate is harmless — keep-N pruning reaps the older one — and it keeps
// LatestDraftSnapshot pointing at the superseding event.
func emitSupersedingDraftSnapshot(ctx context.Context, store *state.Store, namespaceID, path, newRef string, byteSize, fileCount int64) (state.Event, error) {
	payload := dssync.DraftSnapshotPayload{Path: path, BlobRef: newRef, ByteSize: byteSize, FileCount: fileCount}
	raw, err := json.Marshal(payload)
	if err != nil {
		return state.Event{}, err
	}
	var ev state.Event
	// P6-DATA-01: record the origin's superseding snapshot row atomically with
	// the event so GC sees the new ciphertext as retained before hub cleanup.
	err = store.WithTx(ctx, func(tx *state.Tx) error {
		var err error
		ev, err = store.InsertLocalEventTx(ctx, tx, dssync.NewDraftSnapshotEvent(dssync.EventDraftSnapshotCreated, string(raw)))
		if err != nil {
			return err
		}
		return tx.RecordDraftSnapshotTx(ctx, namespaceID, newRef, byteSize, fileCount, ev)
	})
	if err != nil {
		return state.Event{}, err
	}
	return ev, nil
}

// drainPendingHubDeletes deletes blobs queued by a prior local-only revoke
// (P5-PROD-02). It MUST run after the cycle has pushed local events and
// referenced blobs, so the superseding event + new blob are already on the hub
// before the old ciphertext is removed. A still-referenced ref is kept.
func drainPendingHubDeletes(ctx context.Context, store *state.Store, hub dssync.Hub) (int, error) {
	refs, err := store.PendingHubDeletes(ctx)
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, ref := range refs {
		if blobRefStillReferenced(ctx, store, ref) {
			// The old ref came back into use; drop it from the queue without
			// deleting.
			_ = store.ClearPendingHubDelete(ctx, ref)
			continue
		}
		if err := hub.DeleteBlob(ctx, blobHashHex(ref)); err != nil {
			logging.Logger(ctx).Warn("drain: failed to delete queued hub blob", "ref", ref, "err", err.Error())
			continue
		}
		if err := store.ClearPendingHubDelete(ctx, ref); err != nil {
			logging.Logger(ctx).Warn("drain: failed to clear queue entry", "ref", ref, "err", err.Error())
			continue
		}
		deleted++
	}
	return deleted, nil
}

// fetchBlobForRewrap pulls a blob from the hub and verifies its content-address
// (SEC-03) before rewrapping, so a tampered hub cannot inject bytes that get
// re-encrypted to the reduced recipient set.
func fetchBlobForRewrap(ctx context.Context, hub dssync.Hub, ref string) ([]byte, error) {
	reader, err := hub.GetBlob(ctx, blobHashHex(ref))
	if err != nil {
		return nil, fmt.Errorf("get blob %s: %w", ref, err)
	}
	ciphertext, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		return nil, fmt.Errorf("read blob %s: %w", ref, err)
	}
	if err := verifyBlobContentHash(ref, ciphertext); err != nil {
		return nil, fmt.Errorf("verify blob %s: %w", ref, err)
	}
	return ciphertext, nil
}

// blobRefStillReferenced reports whether any secret binding or draft snapshot
// still references ref. It is the safety guard that prevents deleting a hub blob
// another event still points at (SEC-01).
func blobRefStillReferenced(ctx context.Context, store *state.Store, ref string) bool {
	refs, err := store.AllBlobRefs(ctx)
	if err != nil {
		// Conservative: if we cannot verify, do not delete.
		return true
	}
	for _, r := range refs {
		if r == ref {
			return true
		}
	}
	return false
}

// rewrapHubCleanup makes the rewrapped blob discoverable on the hub before
// reclaiming the old ciphertext (P5-SEC-01). Ordering is the whole point:
//  1. push the superseding event(s) so peers can reconstruct newRef;
//  2. push the rewrapped blob;
//  3. only once BOTH are durably on the hub AND no binding/snapshot still
//     references the old ref, delete the old ciphertext.
//
// If any step fails the old ciphertext is kept so the hub never ends up with
// neither a usable event nor a usable blob. Failures are logged, not fatal, so a
// single blob's hub hiccup does not abort the whole revoke rewrap pass.
func rewrapHubCleanup(ctx context.Context, hub dssync.Hub, store *state.Store, oldRef, newRef string, newCiphertext []byte, events []state.Event) {
	if len(events) > 0 {
		if err := hub.Push(ctx, events); err != nil {
			logging.Logger(ctx).Warn("rewrap: failed to push superseding event(s) to hub; keeping old ciphertext", "ref", newRef, "err", err.Error())
			return
		}
	}
	if err := hub.PutBlob(ctx, blobHashHex(newRef), bytes.NewReader(newCiphertext)); err != nil {
		logging.Logger(ctx).Warn("rewrap: failed to push rewrapped blob to hub; keeping old ciphertext", "ref", newRef, "err", err.Error())
		return
	}
	if blobRefStillReferenced(ctx, store, oldRef) {
		logging.Logger(ctx).Warn("rewrap: old blob still referenced, not deleting from hub", "ref", oldRef)
		return
	}
	if err := hub.DeleteBlob(ctx, blobHashHex(oldRef)); err != nil {
		logging.Logger(ctx).Warn("rewrap: failed to delete old blob from hub", "ref", oldRef, "err", err.Error())
	}
}

// gcUnreferencedBlobs removes locally-cached blobs that are no longer
// referenced by any secret binding or draft snapshot (HUB-05). The
// retention/snapshot-horizon gate is deferred until full-state snapshot
// exchange exists; for now this only reclaims blobs with a zero ref-count.
func gcUnreferencedBlobs(ctx context.Context, store *state.Store, paths config.Paths) (int, error) {
	counts, err := store.BlobRefCount(ctx)
	if err != nil {
		return 0, err
	}
	referenced := make(map[string]bool, len(counts))
	for ref := range counts {
		hash, err := envBlobHash(ref)
		if err == nil {
			referenced[hash] = true
		}
	}
	blobDir := filepath.Join(paths.Home, "blobs")
	entries, err := os.ReadDir(blobDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read blob dir: %w", err)
	}
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		hash := strings.TrimSuffix(name, ".age")
		if !strings.HasSuffix(name, ".age") || len(hash) != 64 {
			continue
		}
		if referenced[hash] {
			continue
		}
		//nolint:gosec // The path is under the DevStrap blobs dir and the name is a validated hex hash.
		if err := os.Remove(filepath.Join(blobDir, name)); err != nil {
			logging.Logger(ctx).Warn("gc: failed to remove unreferenced blob", "name", name, "err", err.Error())
			continue
		}
		removed++
	}
	return removed, nil
}
