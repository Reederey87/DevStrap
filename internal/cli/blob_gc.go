package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Reederey87/DevStrap/internal/config"
	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/envbundle"
	"github.com/Reederey87/DevStrap/internal/logging"
	"github.com/Reederey87/DevStrap/internal/platform"
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
	identity, err := devicekeys.NewHybridStore(opts.paths().KeyDir(), platform.Detect().Keychain).Read(ctx, device.ID)
	if err != nil {
		return 0, fmt.Errorf("read local device identity: %w", err)
	}
	refs, err := store.AllBlobRefs(ctx)
	if err != nil {
		return 0, err
	}
	rewrapped := 0
	for _, ref := range refs {
		ciphertext, err := readEnvBlob(opts.paths(), ref)
		if err != nil {
			// SEC-01: pull blobs not cached locally from the hub before
			// rewrapping. Without this, a non-cached blob stays encrypted to
			// the revoked device's key on the hub forever.
			if hub == nil {
				logging.Logger(ctx).Warn("rewrap: blob not cached locally and no hub provided, skipping (hub-side cleanup deferred)", "ref", ref)
				continue
			}
			fetched, ferr := fetchBlobForRewrap(ctx, hub, ref)
			if ferr != nil {
				logging.Logger(ctx).Warn("rewrap: could not fetch blob from hub, skipping", "ref", ref, "err", ferr.Error())
				continue
			}
			ciphertext = fetched
		}
		newCiphertext, newRef, err := envbundle.Rewrap(ciphertext, identity.Private, recipients)
		if err != nil {
			logging.Logger(ctx).Warn("rewrap: decrypt/re-encrypt failed, skipping", "ref", ref, "err", err.Error())
			continue
		}
		if err := writeEnvBlob(opts.paths(), newRef, newCiphertext); err != nil {
			return rewrapped, fmt.Errorf("write rewrapped blob: %w", err)
		}
		if err := store.UpdateBlobRef(ctx, ref, newRef); err != nil {
			return rewrapped, fmt.Errorf("update blob ref: %w", err)
		}
		// SEC-01: push the rewrapped blob to the hub and delete the old
		// ciphertext so the revoked device can no longer fetch it. The old blob
		// is deleted only once no binding/snapshot still references it AND the
		// rewrapped blob was successfully pushed — otherwise deleting the old
		// ciphertext would leave the hub with neither copy (data loss for any
		// device that later needs to fetch it).
		if hub != nil {
			rewrapHubCleanup(ctx, hub, store, ref, newRef, newCiphertext)
		}
		rewrapped++
	}
	return rewrapped, nil
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

// rewrapHubCleanup pushes the rewrapped blob to the hub and, only on a
// successful push AND when no binding still references the old ref, deletes the
// old ciphertext (SEC-01). If the push fails the old ciphertext is kept so the
// hub never ends up with neither copy. Failures are logged, not fatal, so a
// single blob's hub hiccup does not abort the whole revoke rewrap pass.
func rewrapHubCleanup(ctx context.Context, hub dssync.Hub, store *state.Store, oldRef, newRef string, newCiphertext []byte) {
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
