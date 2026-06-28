package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Reederey87/DevStrap/internal/config"
	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/envbundle"
	"github.com/Reederey87/DevStrap/internal/logging"
	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/state"
)

// rewrapBlobsOnRevoke re-encrypts every referenced blob to the current approved
// recipient set (which no longer includes the revoked device) and repoints the
// references (HUB-04). age has no native revocation, so a blob already
// encrypted to a revoked device's X25519 recipient stays decryptable by that key
// forever — re-encryption to the reduced recipient set limits future exposure,
// and the underlying secrets are already flagged needs_rotation by the revoke
// command.
func rewrapBlobsOnRevoke(ctx context.Context, store *state.Store, opts *options) (int, error) {
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
			logging.Logger(ctx).Warn("rewrap: blob not cached locally, skipping", "ref", ref)
			continue
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
		rewrapped++
	}
	return rewrapped, nil
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
