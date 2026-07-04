package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/Reederey87/DevStrap/internal/config"
	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
	"github.com/Reederey87/DevStrap/internal/workspacekeys"
)

// recoverFromSnapshot performs a full-state snapshot exchange when a hub's
// retention floor has passed this device's pull cursor (P4-SYNC-02). It replaces
// the old ErrSnapshotRequired dead-end: get + fail-closed-verify the retention
// manifest, pull the tail (so an in-batch grant is ingested before we unseal),
// fetch + sha-check + unseal the snapshot, ImportSnapshot (which advances the
// per-device cursors to the floor), then pull the imported draft blobs. The
// caller re-runs the normal incremental pull afterward, which now succeeds.
//
// It returns imported=true when state was actually replaced, imported=false with
// a nil error only for the keyless-joiner defer (the snapshot is sealed under an
// epoch this device does not hold yet — awaiting a grant, retry next sync).
// Trust refusals are classified exitInvalidConfig and hub/fetch failures
// exitNetwork; local store/keyring failures deliberately keep the default
// class, matching the rest of the sync path.
func recoverFromSnapshot(ctx context.Context, stdout io.Writer, store *state.Store, hub dssync.Hub, hubID string, paths config.Paths, keyring *workspacekeys.Keyring) (bool, error) {
	// 1. Fetch the retention manifest.
	raw, _, err := hub.GetRetention(ctx)
	if err != nil {
		if errors.Is(err, dssync.ErrRetentionNotFound) {
			return false, appError{code: exitNetwork, err: fmt.Errorf("hub demands a snapshot but has no retention manifest — hub is inconsistent")}
		}
		return false, appError{code: exitNetwork, err: fmt.Errorf("read retention manifest: %w", err)}
	}
	m, err := dssync.ParseRetentionManifest(raw)
	if err != nil {
		return false, appError{code: exitNetwork, err: fmt.Errorf("parse retention manifest: %w", err)}
	}

	// 2. Fail-closed trust check: the producer must be a locally pinned approved
	// device with a stored signing key, then the manifest signature must verify.
	pub, ok, err := store.ApprovedDeviceSigningKey(ctx, m.ProducedBy)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, appError{code: exitInvalidConfig, err: fmt.Errorf(
			"%w: snapshot producer %s is not a locally approved device; pin/enroll it (devstrap devices enroll … / devstrap init --join --code …) or run `devstrap hub compact` from a device this machine trusts",
			dssync.ErrSnapshotVerification, m.ProducedBy)}
	}
	if err := dssync.VerifyRetentionManifest(m, pub); err != nil {
		return false, appError{code: exitInvalidConfig, err: err}
	}
	// A compactor signs a manifest naming its OWN snapshot: signer and snapshot
	// producer are the same device by protocol. A manifest signed by approved
	// device A that names device B's snapshot is outside the protocol and must
	// not let B's payload ride A's signature (post-review Codex P2).
	if m.Snapshot.ProducedBy != m.ProducedBy {
		return false, appError{code: exitInvalidConfig, err: fmt.Errorf(
			"%w: manifest signed by %s names a snapshot produced by %s — signer and producer must match",
			dssync.ErrSnapshotVerification, m.ProducedBy, m.Snapshot.ProducedBy)}
	}

	// 3. Floor-rollback guard. A floor is monotonic by protocol; if the served
	// manifest walks any device's floor below one we have already verified, warn
	// loudly and use the higher cached floor for the cursor math below.
	cached, err := dssync.LoadRetentionFloorCache(ctx, store, hubID)
	if err != nil {
		return false, err
	}
	effectiveFloors := map[string]int64{}
	for dev, seq := range m.Floors {
		effectiveFloors[dev] = seq
	}
	for dev, c := range cached {
		if c > effectiveFloors[dev] {
			_, _ = fmt.Fprintf(stdout, "warning: hub retention floor for device %s rolled back from %d to %d; using the higher verified floor\n",
				dev, c, effectiveFloors[dev])
			effectiveFloors[dev] = c
		}
	}

	// 4. Pull the tail FIRST from max(cursor, floor-1) per device. This ingests
	// any in-batch grant (via EncryptedHub) so a fresh joiner holds the WCK before
	// we unseal. The pull is provisional — its cursor is NOT persisted; the import
	// sets cursors, and the caller's normal pull applies the tail.
	cursorMap, err := store.HubDeviceCursors(ctx, hubID)
	if err != nil {
		return false, err
	}
	tailCursor := dssync.Cursor{}
	for dev, seq := range cursorMap {
		tailCursor[dev] = seq
	}
	for dev, floor := range effectiveFloors {
		if floor-1 > tailCursor[dev] {
			tailCursor[dev] = floor - 1
		}
	}
	if _, err := hub.Pull(ctx, tailCursor); err != nil {
		if errors.Is(err, dssync.ErrSnapshotRequired) {
			return false, appError{code: exitNetwork, err: fmt.Errorf("retention floor moved during recovery — re-run devstrap sync")}
		}
		return false, appError{code: exitNetwork, err: fmt.Errorf("pull snapshot tail: %w", err)}
	}

	// 5. Fetch + verify + unseal the snapshot object.
	obj, err := hub.GetSnapshotObject(ctx, m.Snapshot.SHA256)
	if err != nil {
		return false, appError{code: exitNetwork, err: fmt.Errorf("fetch snapshot object: %w", err)}
	}
	sum := sha256.Sum256(obj)
	if got := hex.EncodeToString(sum[:]); got != m.Snapshot.SHA256 {
		return false, appError{code: exitInvalidConfig, err: fmt.Errorf(
			"%w: snapshot object sha256 %s does not match manifest %s (hub tampering?)", dssync.ErrSnapshotVerification, got, m.Snapshot.SHA256)}
	}
	info, err := dssync.ParseSnapshotEnvelope(obj)
	if err != nil {
		return false, appError{code: exitInvalidConfig, err: err}
	}
	if info.ProducedBy != m.Snapshot.ProducedBy || info.Epoch != m.Snapshot.Epoch || info.KID != m.Snapshot.KID || info.HLC != m.Snapshot.HLC {
		return false, appError{code: exitInvalidConfig, err: fmt.Errorf(
			"%w: snapshot envelope (producer %s, epoch %d, kid %s, hlc %d) disagrees with the signed manifest (producer %s, epoch %d, kid %s, hlc %d)",
			dssync.ErrSnapshotVerification, info.ProducedBy, info.Epoch, info.KID, info.HLC,
			m.Snapshot.ProducedBy, m.Snapshot.Epoch, m.Snapshot.KID, m.Snapshot.HLC)}
	}
	if err := keyring.Prime(ctx); err != nil {
		return false, fmt.Errorf("recover: prime keyring: %w", err)
	}
	candidates := wckCandidates(keyring, info.Epoch, info.KID)
	if len(candidates) == 0 {
		// Keyless joiner: no WCK at this epoch. Defer without importing; the next
		// sync retries once the grant lands. Mirrors the pull-side awaiting-grant
		// defer (exit 0).
		_, _ = fmt.Fprintf(stdout, "Awaiting workspace key grant: the hub snapshot is sealed under epoch %d, which this device does not hold yet. "+
			"Approve this device on another machine (devstrap devices approve <id>), then re-run sync.\n", info.Epoch)
		return false, nil
	}
	var snap dssync.Snapshot
	var unsealed bool
	for _, wck := range candidates {
		s, uerr := dssync.UnsealSnapshot(obj, wck)
		if uerr == nil {
			snap, unsealed = s, true
			break
		}
	}
	if !unsealed {
		// Candidates existed but every AEAD open failed: corruption or a forged
		// carrier, never a missing grant. Fail closed.
		return false, appError{code: exitInvalidConfig, err: fmt.Errorf(
			"%w: snapshot object failed to open under every held key at epoch %d", dssync.ErrSnapshotVerification, info.Epoch)}
	}

	// 6. Cross-check the sealed document against the local workspace and the
	// signed manifest (the manifest signature covers floors; the AEAD covers the
	// snapshot's own copy — they must agree).
	localWS, err := store.WorkspaceID(ctx)
	if err != nil {
		return false, err
	}
	if snap.WorkspaceID != localWS {
		return false, appError{code: exitInvalidConfig, err: fmt.Errorf(
			"%w: snapshot workspace %s does not match local workspace %s", dssync.ErrSnapshotVerification, snap.WorkspaceID, localWS)}
	}
	// The sealed document's own identity fields must match the envelope (and
	// therefore the signed manifest, already checked above). SealSnapshot stamps
	// them from one struct, so a mismatch means a WCK holder handcrafted a
	// divergent inner document — defense in depth (post-review Codex P2).
	if snap.ProducedBy != info.ProducedBy || snap.HLC != info.HLC || snap.Epoch != info.Epoch || snap.KID != info.KID {
		return false, appError{code: exitInvalidConfig, err: fmt.Errorf(
			"%w: sealed snapshot document identity disagrees with its envelope", dssync.ErrSnapshotVerification)}
	}
	if !floorsEqual(snap.Floor, m.Floors) {
		return false, appError{code: exitInvalidConfig, err: fmt.Errorf(
			"%w: snapshot floors disagree with the signed manifest floors", dssync.ErrSnapshotVerification)}
	}

	// 7. Import (advances the per-device cursors to the floor and caches it).
	if err := dssync.ImportSnapshot(ctx, store, snap, m.Snapshot.SHA256, hubID); err != nil {
		return false, fmt.Errorf("import snapshot: %w", err)
	}

	// 8. Pull the blobs referenced by imported draft pointers — they have no
	// carrier event on the tail, so the normal per-event blob pull will not see
	// them.
	var draftRefs []string
	for _, entry := range snap.Entries {
		if entry.Draft != nil && entry.Draft.BlobRef != "" {
			draftRefs = append(draftRefs, entry.Draft.BlobRef)
		}
	}
	if missing, berr := pullBlobsByRef(ctx, hub, draftRefs, paths); berr != nil {
		return false, appError{code: exitNetwork, err: fmt.Errorf("pull imported draft blobs: %w", berr)}
	} else if missing > 0 {
		_, _ = fmt.Fprintf(stdout, "warning: %d imported draft blob(s) missing from hub; materialization may be incomplete\n", missing)
	}
	return true, nil
}

// wckCandidates mirrors EncryptedHub.Pull's candidate selection: the exact-kid
// key first (when the envelope names one), then every held key at the epoch, so
// the AEAD authenticates against the right key regardless of an unauthenticated
// kid hint.
func wckCandidates(keyring *workspacekeys.Keyring, epoch int64, kid string) [][]byte {
	var candidates [][]byte
	if kid != "" {
		candidates = append(candidates, keyring.WCKCandidates(epoch, kid)...)
	}
	candidates = append(candidates, keyring.WCKCandidates(epoch, "")...)
	return candidates
}

// floorsEqual reports whether two per-device floor maps are identical.
func floorsEqual(a dssync.Cursor, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for dev, seq := range a {
		if b[dev] != seq {
			return false
		}
	}
	return true
}
