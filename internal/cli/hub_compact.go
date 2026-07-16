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
	"sort"

	"github.com/Reederey87/DevStrap/internal/config"
	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
	"github.com/spf13/cobra"
)

// hubCompactResult is the --json shape for `hub compact` (and --dry-run) (P5-CLI-01 part B).
type hubCompactResult struct {
	DryRun                    bool             `json:"dry_run,omitempty"`
	SnapshotSHA               string           `json:"snapshot_sha,omitempty"`
	Floors                    map[string]int64 `json:"floors"`
	ColdEventsDeleted         int              `json:"cold_events_deleted,omitempty"`
	ColdEventsEstimate        int              `json:"cold_events_estimate,omitempty"`
	TombstonesGC              int              `json:"tombstones_gc,omitempty"`
	RevokedStreamObjects      int              `json:"revoked_stream_objects,omitempty"`
	SupersededSnapshotsPruned int              `json:"superseded_snapshots_pruned,omitempty"`
	KeepSnapshots             int              `json:"keep_snapshots,omitempty"`
	SnapshotEntries           int              `json:"snapshot_entries,omitempty"`
	SnapshotTombstones        int              `json:"snapshot_tombstones,omitempty"`
	SnapshotAnchors           int              `json:"snapshot_anchors,omitempty"`
	SnapshotBytes             int              `json:"snapshot_bytes,omitempty"`
	TombstoneGCBeforeHLC      int64            `json:"tombstone_gc_before_hlc,omitempty"`
	TombstoneGCSkipped        string           `json:"tombstone_gc_skipped,omitempty"`
}

func newHubCompactCommand(stdout io.Writer, opts *options) *cobra.Command {
	var hubFile string
	var dryRun bool
	var keepSnapshots int
	var minEvents int
	var gcTombstones bool
	cmd := &cobra.Command{
		Use:   "compact",
		Short: "Publish a full-state snapshot, advance the retention floors, and delete cold events (P4-HUB-11)",
		Long: `Compact the hub event log: converge (pull + apply + push), publish a signed,
sealed full-state snapshot that covers everything below the new per-device
retention floors, advance the floors via a compare-and-swap retention manifest,
confirm the write by read-back, and only THEN delete the now-cold events below
the floors.

The order is confirm-before-delete: a crash anywhere leaves a superset of the
committed state (safe). Floors are monotonic — compact refuses to lower any
device's floor or to build on a retention manifest it cannot verify. A device
that has fallen below a floor recovers automatically on its next sync by
importing the snapshot.

compact only advances floors from a fully-synced, complete replica: it refuses
while any pulled event was deferred, skipped, or quarantined, while any
quarantine-class conflict or skipped event is open, or while any workspace key
grant is still awaited.

Concurrent destructive hub passes (gc / compact / migrate-events) on cooperating
clients are serialized by an advisory sweep lock; a hostile writer is out of
scope (spec/15). A keyless device cannot compact — the snapshot is sealed under
the current-epoch workspace key.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			hub, hubID, err := hubFromOptions(cmd.Context(), opts, store, hubFile)
			if err != nil {
				return appError{code: exitInvalidConfig, err: err}
			}
			return hubCompact(cmd.Context(), stdout, cmd.ErrOrStderr(), opts, store, hub, hubID, opts.paths(), keepSnapshots, minEvents, gcTombstones, dryRun)
		},
	}
	cmd.Flags().StringVar(&hubFile, "hub-file", "", "file-backed test hub path")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "compute and print the compaction plan without writing anything to the hub")
	cmd.Flags().IntVar(&keepSnapshots, "keep-snapshots", 2, "snapshot objects to retain on the hub (>=1; the newly published one is always kept)")
	cmd.Flags().IntVar(&minEvents, "min-events", 0, "refuse to compact unless at least this many events would be deleted (0 = always compact)")
	cmd.Flags().BoolVar(&gcTombstones, "gc-tombstones", true, "garbage-collect tombstones every approved device has acked (P4-SYNC-06); --gc-tombstones=false retains them")
	return cmd
}

// hubCompact publishes a full-state snapshot and advances the hub's per-device
// retention floors, then deletes the cold events below them (P4-HUB-11). See
// newHubCompactCommand's Long help for the confirm-before-delete contract.
func hubCompact(ctx context.Context, stdout, stderr io.Writer, opts *options, store *state.Store, hub dssync.Hub, hubID string, paths config.Paths, keepSnapshots, minEvents int, gcTombstones, dryRun bool) error {
	if keepSnapshots < 1 {
		keepSnapshots = 1
	}
	device, err := store.CurrentDevice(ctx)
	if err != nil {
		return err
	}
	keyring := buildKeyring(ctx, opts, store)

	// 1a. Converge (pull side) and refuse on any incomplete-view signal EXCEPT an
	// open omission alarm: compact is the documented cure for the permanent
	// per-device gap that alarm reports (P5-SYNC-01 / P4-SYNC-05), so it must not
	// be blocked by the gap's own alarm.
	pull, err := refuseIfIncompleteView(ctx, stderr, store, hub, hubID, paths, keyring, true)
	if err != nil {
		return err
	}

	// 1b. Push local pending events so floors[self] can cover local history.
	// A dry run writes NOTHING to the hub, so it skips the push and previews
	// floors from the current (pre-push) watermark.
	if !dryRun {
		pushCursor, perr := store.PushSeqCursor(ctx, hubID)
		if perr != nil {
			return perr
		}
		localEvents, perr := store.LocalPendingEventsBySeq(ctx, pushCursor)
		if perr != nil {
			return perr
		}
		rawSeen := len(pull.events)
		if eh, ok := hub.(dssync.EncryptedHub); ok && eh.Stats != nil {
			rawSeen = eh.Stats.RawSeen
		}
		_, deferred, ferr := pushLocalEventsGated(ctx, stdout, opts, store, hub, hubID, localEvents, rawSeen)
		if ferr != nil {
			return ferr
		}
		if deferred {
			return appError{code: exitInvalidConfig, err: fmt.Errorf(
				"cannot compact: this device is awaiting a workspace key grant (its local events are queued); approve it and sync, then re-run")}
		}
	}

	// 3. Compute the base per-device floors from the transport cursors.
	baseFloors, err := computeCompactFloors(ctx, store, hubID, device.ID)
	if err != nil {
		return err
	}

	// 4a. Read the current retention manifest for monotonicity, carry-forward,
	// and the PrevSHA256 chain.
	currentRaw, etag, err := hub.GetRetention(ctx)
	switch {
	case errors.Is(err, dssync.ErrRetentionNotFound):
		currentRaw, etag = nil, ""
	case err != nil:
		return appError{code: exitNetwork, err: fmt.Errorf("read retention manifest: %w", err)}
	}

	// 4b. Reconcile against the current manifest (verify producer + signature,
	// enforce monotonicity, carry forward absent devices) to preview floors.
	previewFloors, _, err := reconcileCompactFloors(ctx, store, device, baseFloors, currentRaw)
	if err != nil {
		return err
	}

	// --min-events guard, applied BEFORE any hub write (dry run included, so the
	// preview refuses exactly as a real run would).
	oldFloors := map[string]int64{}
	if len(currentRaw) > 0 {
		if parsed, perr := dssync.ParseRetentionFloors(currentRaw); perr == nil {
			oldFloors = parsed
		}
	}
	estimate := estimateCompactedEvents(previewFloors, oldFloors)
	if minEvents > 0 && estimate < minEvents {
		return appError{code: exitInvalidConfig, err: fmt.Errorf(
			"refusing to compact: only %d event(s) would be deleted, below --min-events %d", estimate, minEvents)}
	}

	// 5. Build the snapshot document from store reads.
	hlc, err := store.CurrentHLC(ctx)
	if err != nil {
		return err
	}

	// P4-SYNC-06 tombstone-GC plan: derive the safe floor from approved devices'
	// signed acks. This is read-only; the actual purge runs BELOW, before the
	// snapshot is built, so GC'd tombstones are excluded from the produced
	// snapshot.
	var gcBeforeHLC int64
	var gcReady bool
	var gcSkip string
	if gcTombstones {
		gcBeforeHLC, gcReady, gcSkip, err = planTombstoneGC(ctx, store, hub, device.ID)
		if err != nil {
			return err
		}
	}

	if dryRun {
		return printCompactPlan(ctx, stdout, opts, store, device.ID, hlc, previewFloors, estimate, keepSnapshots, gcTombstones, gcReady, gcBeforeHLC, gcSkip)
	}

	// P4-HUB-12: serialize the destructive publish/delete sequence behind the
	// advisory sweep lock so a concurrent gc/compact/migrate-events on a
	// cooperating client cannot interleave. The pre-sync (pull + push above) is
	// non-destructive; the lock guards the seal → publish → CAS → delete run
	// below. A dry run returned above without acquiring it.
	release, lerr := hubSweepLock(ctx, store, hub, defaultSweepLockTTL)
	if lerr != nil {
		return lerr
	}
	defer release()

	// 6. Seal under the CURRENT-epoch WCK (a keyless device cannot compact).
	epoch, kid, wck, err := keyring.PushKey(ctx)
	if err != nil {
		return err
	}
	if epoch == 0 || len(wck) == 0 {
		return appError{code: exitInvalidConfig, err: fmt.Errorf(
			"cannot compact: this device holds no workspace key to seal a snapshot under; approve it and sync so it ingests the fleet key, then re-run")}
	}

	// The manifest signature is made with the local device's private signing key
	// (the same identity that signs its events).
	keyStore, err := resolveKeyStore(ctx, paths, store)
	if err != nil {
		return err
	}
	signing, _, err := keyStore.EnsureSigning(ctx, device.ID, device.SigningPublicKey)
	if err != nil {
		return fmt.Errorf("read local signing identity: %w", err)
	}

	// 5b. P4-SYNC-06: purge acked tombstones BEFORE building the snapshot, so the
	// GC'd rows are excluded from the produced snapshot document (BuildSnapshot
	// reads live tombstones from the store). Purging is idempotent, and a purge
	// followed by a failed publish is safe: a GC'd tombstone was below the minimum
	// ack watermark, so no device can resurrect it regardless of the snapshot.
	tombstonesGCd := 0
	if gcTombstones {
		if gcReady {
			tombstonesGCd, err = store.GCTombstones(ctx, gcBeforeHLC)
			if err != nil {
				return err
			}
		} else if gcSkip != "" {
			_, _ = fmt.Fprintf(stderr, "hub compact: retaining tombstones: %s\n", gcSkip)
		}
	}

	// 6/7. One publish attempt: reconcile against the given manifest bytes, build
	// + seal the snapshot under the reconciled floors, PutSnapshotObject, then
	// sign + CAS the retention manifest. Returns the winning manifest and the
	// snapshot sha, or ErrRetentionConflict on a lost CAS.
	publish := func(curRaw []byte, ifMatchETag string) (dssync.RetentionManifest, string, []byte, error) {
		floors, prev, rerr := reconcileCompactFloors(ctx, store, device, baseFloors, curRaw)
		if rerr != nil {
			return dssync.RetentionManifest{}, "", nil, rerr
		}
		snap, berr := dssync.BuildSnapshot(ctx, store, device.ID, hlc, floors)
		if berr != nil {
			return dssync.RetentionManifest{}, "", nil, berr
		}
		obj, sha, serr := dssync.SealSnapshot(snap, wck, epoch)
		if serr != nil {
			return dssync.RetentionManifest{}, "", nil, serr
		}
		if perr := hub.PutSnapshotObject(ctx, sha, obj); perr != nil {
			return dssync.RetentionManifest{}, "", nil, appError{code: exitNetwork, err: fmt.Errorf("put snapshot object: %w", perr)}
		}
		m := dssync.RetentionManifest{
			WorkspaceID: snap.WorkspaceID,
			Floors:      map[string]int64(floors),
			Snapshot: dssync.RetentionSnapshotRef{
				Epoch: epoch, HLC: hlc, KID: kid, ProducedBy: device.ID, SHA256: sha,
			},
			ProducedBy: device.ID,
			ProducedAt: hlc,
			PrevSHA256: prev,
		}
		if serr := dssync.SignRetentionManifest(&m, signing.Private); serr != nil {
			return dssync.RetentionManifest{}, "", nil, serr
		}
		raw, merr := json.Marshal(m)
		if merr != nil {
			return dssync.RetentionManifest{}, "", nil, fmt.Errorf("marshal retention manifest: %w", merr)
		}
		return m, sha, raw, hub.PutRetention(ctx, raw, ifMatchETag)
	}

	manifest, snapSHA, manifestRaw, err := publish(currentRaw, etag)
	if errors.Is(err, dssync.ErrRetentionConflict) {
		// Re-read ONCE, re-reconcile, retry the CAS once. A second conflict is an
		// error — some other device is compacting concurrently.
		curRaw2, etag2, gerr := hub.GetRetention(ctx)
		switch {
		case errors.Is(gerr, dssync.ErrRetentionNotFound):
			curRaw2, etag2 = nil, ""
		case gerr != nil:
			return appError{code: exitNetwork, err: fmt.Errorf("re-read retention manifest after CAS conflict: %w", gerr)}
		}
		manifest, snapSHA, manifestRaw, err = publish(curRaw2, etag2)
		if errors.Is(err, dssync.ErrRetentionConflict) {
			return appError{code: exitNetwork, err: fmt.Errorf(
				"retention manifest changed twice during compaction — another device is compacting concurrently; re-run `devstrap hub compact` later")}
		}
	}
	if err != nil {
		return err
	}

	// 8. Read-back confirm: the hub must serve back the EXACT manifest bytes we
	// wrote — a byte-for-byte comparison, not a sha-of-snapshot match. A hostile
	// hub could ack the CAS and then serve a forged manifest that merely names
	// the same snapshot sha; deletion below is gated on the confirmed bytes so
	// forged floors can never widen what we delete (post-review Codex P1).
	rbRaw, _, err := hub.GetRetention(ctx)
	if err != nil {
		return appError{code: exitNetwork, err: fmt.Errorf("read back retention manifest: %w", err)}
	}
	if !bytes.Equal(rbRaw, manifestRaw) {
		return appError{code: exitNetwork, err: fmt.Errorf(
			"retention manifest read-back does not match what this device just wrote — another device raced us or the hub is serving stale/forged bytes; nothing was deleted, re-run `devstrap hub compact`")}
	}

	// 9. Only now is deletion safe: delete cold events below the committed floors.
	deleted, err := hub.CompactEventsBelow(ctx, dssync.Cursor(manifest.Floors))
	if err != nil {
		return appError{code: exitNetwork, err: fmt.Errorf("delete cold events: %w", err)}
	}

	// Advance our own pull cursors to the floors (forward-only). We originated or
	// have already consumed everything the floors cover — in particular our own
	// stream, whose pull cursor would otherwise sit below floor[self] and force
	// the NEXT sync to demand a snapshot of our own state (which self-recovery
	// refuses). This mirrors ImportSnapshot's cursor advance.
	for dev, floor := range manifest.Floors {
		if floor > 1 {
			if aerr := store.AdvanceHubDeviceCursor(ctx, hubID, dev, floor-1); aerr != nil {
				return aerr
			}
		}
	}

	// 9b. P4-SYNC-06 revoked-stream cleanup: for a revoked/lost device whose
	// stream the compactor fully consumed (present in the committed floors),
	// reclaim its entire event-log prefix and delete its stale ack.
	revokedObjs, err := cleanupRevokedStreams(ctx, stderr, store, hub, manifest.Floors)
	if err != nil {
		return err
	}

	// 10. Prune superseded snapshot objects, keeping the manifest-referenced one
	// plus the newest keepSnapshots-1 others.
	prunedSnaps, err := pruneSnapshotObjects(ctx, stderr, hub, snapSHA, keepSnapshots)
	if err != nil {
		return err
	}

	result := hubCompactResult{
		SnapshotSHA:               snapSHA,
		Floors:                    manifest.Floors,
		ColdEventsDeleted:         deleted,
		TombstonesGC:              tombstonesGCd,
		RevokedStreamObjects:      revokedObjs,
		SupersededSnapshotsPruned: prunedSnaps,
		KeepSnapshots:             keepSnapshots,
	}
	return opts.render(stdout, func(w io.Writer) error {
		if _, err := fmt.Fprintf(w, "hub compact: published snapshot %s; advanced %d device floor(s); deleted %d cold event(s); GC'd %d tombstone(s); reclaimed %d revoked-stream object(s); pruned %d superseded snapshot(s)\n",
			shortSHA(snapSHA), len(manifest.Floors), deleted, tombstonesGCd, revokedObjs, prunedSnaps); err != nil {
			return err
		}
		return printFloors(w, manifest.Floors)
	}, result)
}

// planTombstoneGC derives the safe tombstone-GC floor from signed sync acks
// (P4-SYNC-06): the minimum HLC watermark across the LOCAL device's live clock
// and EVERY approved non-local device's verified ack. Every approved non-local
// device must have a verified ack, else GC is skipped (ready=false, skip set
// with a hint) — a tombstone must never be purged before a peer that could still
// resurrect it has consumed the delete. Acks from non-approved (revoked/lost/
// pending/unknown) devices and acks that fail verification are ignored: they can
// neither pin nor advance the floor. Listing the acks is a hub read, so a hub
// error is classified exitNetwork; local store failures keep the default class.
func planTombstoneGC(ctx context.Context, store *state.Store, hub dssync.Hub, selfDeviceID string) (beforeHLC int64, ready bool, skip string, err error) {
	localWatermark, err := store.CurrentHLC(ctx)
	if err != nil {
		return 0, false, "", err
	}
	workspaceID, err := store.WorkspaceID(ctx)
	if err != nil {
		return 0, false, "", err
	}
	rawAcks, err := hub.ListAcks(ctx)
	if err != nil {
		return 0, false, "", appError{code: exitNetwork, err: fmt.Errorf("list sync acks: %w", err)}
	}
	// Verify every ack against the local registry; index the survivors by device.
	verified := map[string]dssync.AckMarker{}
	for dev, raw := range rawAcks {
		if dev == selfDeviceID {
			continue // the local device contributes its live watermark, not an ack
		}
		pub, ok, aerr := store.ApprovedDeviceSigningKey(ctx, dev)
		if aerr != nil {
			return 0, false, "", aerr
		}
		if !ok {
			continue // revoked/lost/pending/unknown — ignore
		}
		m, perr := dssync.ParseAckMarker(raw)
		if perr != nil {
			continue // unparseable — ignore (its owner overwrites it next sync)
		}
		if m.DeviceID != dev || m.WorkspaceID != workspaceID {
			continue // key/payload device mismatch or wrong workspace — ignore
		}
		if verr := dssync.VerifyAckMarker(m, pub); verr != nil {
			continue // bad signature — ignore
		}
		verified[dev] = m
	}
	// Require a verified ack from every APPROVED non-local device.
	devices, err := store.ListDevices(ctx)
	if err != nil {
		return 0, false, "", err
	}
	minWatermark := localWatermark
	for _, d := range devices {
		if d.ID == selfDeviceID || d.TrustState != "approved" {
			continue
		}
		m, ok := verified[d.ID]
		if !ok {
			return 0, false, fmt.Sprintf("device %s has never written a verified sync ack", d.ID), nil
		}
		if m.HLCWatermark < minWatermark {
			minWatermark = m.HLCWatermark
		}
	}
	return minWatermark, true, "", nil
}

// cleanupRevokedStreams reclaims the event-log prefix and stale ack of every
// revoked/lost device whose stream the just-committed floors fully cover
// (P4-SYNC-06). The device's floor and local pull cursor are deliberately
// RETAINED: a floor + cursor for a now-empty stream is harmless, and deleting
// the cursor while the floor stays would reopen the retention gate and force a
// needless snapshot recovery on the next sync. Best-effort per device — a
// failure warns and continues. Returns the total object count reclaimed.
func cleanupRevokedStreams(ctx context.Context, stderr io.Writer, store *state.Store, hub dssync.Hub, floors map[string]int64) (int, error) {
	devices, err := store.ListDevices(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, d := range devices {
		if d.TrustState != "revoked" && d.TrustState != "lost" {
			continue
		}
		if floor, ok := floors[d.ID]; !ok || floor <= 0 {
			continue // never consumed / no floor — nothing safely deletable
		}
		n, serr := hub.DeleteDeviceStream(ctx, d.ID)
		if serr != nil {
			_, _ = fmt.Fprintf(stderr, "warning: failed to reclaim revoked device %s stream: %v\n", d.ID, serr)
			continue
		}
		if aerr := hub.DeleteAck(ctx, d.ID); aerr != nil {
			_, _ = fmt.Fprintf(stderr, "warning: failed to delete revoked device %s ack: %v\n", d.ID, aerr)
		}
		total += n
	}
	return total, nil
}

// computeCompactFloors derives the base per-device retention floors from the
// transport cursors (P4-HUB-11): for each remote device with a non-zero pull
// cursor, floor = cursor+1; for the local device, floor = pushWatermark+1 (the
// push watermark, never the pull cursor, governs this device's own stream). A
// device with cursor 0 has consumed nothing and gets no floor.
func computeCompactFloors(ctx context.Context, store *state.Store, hubID, selfDeviceID string) (dssync.Cursor, error) {
	cursors, err := store.HubDeviceCursors(ctx, hubID)
	if err != nil {
		return nil, err
	}
	push, err := store.PushSeqCursor(ctx, hubID)
	if err != nil {
		return nil, err
	}
	floors := dssync.Cursor{}
	for dev, seq := range cursors {
		if dev == selfDeviceID || seq <= 0 {
			continue
		}
		floors[dev] = seq + 1
	}
	if push > 0 {
		floors[selfDeviceID] = push + 1
	}
	return floors, nil
}

// reconcileCompactFloors verifies the current retention manifest (its producer
// must be the local device or a locally approved device, with a verifying
// signature), enforces floor monotonicity (refusing any device whose new floor
// is below the current one), and carries forward the floor of any device present
// in the current manifest but absent from ours. It returns the reconciled floors
// and the PrevSHA256 (the sha256 of the current manifest bytes, "" when none
// exists) for the chain.
func reconcileCompactFloors(ctx context.Context, store *state.Store, self state.Device, base dssync.Cursor, currentRaw []byte) (dssync.Cursor, string, error) {
	floors := dssync.Cursor{}
	for dev, seq := range base {
		floors[dev] = seq
	}
	if len(currentRaw) == 0 {
		return floors, "", nil
	}
	m, err := dssync.ParseRetentionManifest(currentRaw)
	if err != nil {
		return nil, "", appError{code: exitNetwork, err: fmt.Errorf("parse current retention manifest: %w", err)}
	}
	// The producer must be verifiable: the local device (we produced our own
	// prior manifest, and the local row is 'local' not 'approved') or a locally
	// approved peer.
	var pub string
	if m.ProducedBy == self.ID {
		pub = self.SigningPublicKey
		if pub == "" {
			return nil, "", appError{code: exitInvalidConfig, err: fmt.Errorf(
				"%w: the local device has no recorded signing key to verify its own prior retention manifest", dssync.ErrSnapshotVerification)}
		}
	} else {
		approved, ok, aerr := store.ApprovedDeviceSigningKey(ctx, m.ProducedBy)
		if aerr != nil {
			return nil, "", aerr
		}
		if !ok {
			return nil, "", appError{code: exitInvalidConfig, err: fmt.Errorf(
				"%w: the current retention manifest was produced by %s, which is not a locally approved device; refusing to build a new manifest on an unverifiable one",
				dssync.ErrSnapshotVerification, m.ProducedBy)}
		}
		pub = approved
	}
	if verr := dssync.VerifyRetentionManifest(m, pub); verr != nil {
		return nil, "", appError{code: exitInvalidConfig, err: verr}
	}
	for dev, nf := range floors {
		if of, ok := m.Floors[dev]; ok && nf < of {
			return nil, "", appError{code: exitInvalidConfig, err: fmt.Errorf(
				"%w: new floor for device %s (%d) is below the current manifest floor (%d); floors are monotonic",
				dssync.ErrRetentionRollback, dev, nf, of)}
		}
	}
	for dev, of := range m.Floors {
		if _, ok := floors[dev]; !ok {
			floors[dev] = of
		}
	}
	sum := sha256.Sum256(currentRaw)
	return floors, hex.EncodeToString(sum[:]), nil
}

// estimateCompactedEvents estimates how many events a compaction would delete:
// per device, the count of sequence numbers between the previous floor and the
// new one, i.e. sum over devices of max(0, newFloor - oldFloor), where a device
// absent from the old manifest has a previous floor of 1 (nothing deleted yet).
func estimateCompactedEvents(newFloors dssync.Cursor, oldFloors map[string]int64) int {
	total := 0
	for dev, nf := range newFloors {
		of := int64(1)
		if v, ok := oldFloors[dev]; ok {
			of = v
		}
		if nf > of {
			total += int(nf - of)
		}
	}
	return total
}

// pruneSnapshotObjects deletes superseded snapshot objects, keeping the
// manifest-referenced one plus the newest keepSnapshots-1 others by
// LastModified.
func pruneSnapshotObjects(ctx context.Context, stderr io.Writer, hub dssync.Hub, referencedSHA string, keepSnapshots int) (int, error) {
	objs, err := hub.ListSnapshotObjects(ctx)
	if err != nil {
		return 0, appError{code: exitNetwork, err: fmt.Errorf("list snapshot objects: %w", err)}
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].LastModified.After(objs[j].LastModified) })
	keep := map[string]bool{referencedSHA: true}
	extra := keepSnapshots - 1
	for _, o := range objs {
		if o.Key == referencedSHA {
			continue
		}
		if extra > 0 {
			keep[o.Key] = true
			extra--
		}
	}
	pruned := 0
	for _, o := range objs {
		if keep[o.Key] {
			continue
		}
		if derr := hub.DeleteSnapshotObject(ctx, o.Key); derr != nil {
			_, _ = fmt.Fprintf(stderr, "warning: failed to delete superseded snapshot %s: %v\n", shortSHA(o.Key), derr)
			continue
		}
		pruned++
	}
	return pruned, nil
}

// printCompactPlan renders the dry-run plan: the per-device floors, the
// event-delete estimate, the snapshot document size, the retention policy, and
// the tombstone-GC decision (the safe floor derived from acks, or the reason it
// is skipped). Under --json it encodes hubCompactResult instead.
func printCompactPlan(ctx context.Context, stdout io.Writer, opts *options, store *state.Store, producedBy string, hlc int64, floors dssync.Cursor, estimate, keepSnapshots int, gcTombstones, gcReady bool, gcBeforeHLC int64, gcSkip string) error {
	snap, err := dssync.BuildSnapshot(ctx, store, producedBy, hlc, floors)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal snapshot plan: %w", err)
	}
	result := hubCompactResult{
		DryRun:             true,
		Floors:             map[string]int64(floors),
		ColdEventsEstimate: estimate,
		KeepSnapshots:      keepSnapshots,
		SnapshotEntries:    len(snap.Entries),
		SnapshotTombstones: len(snap.Tombstones),
		SnapshotAnchors:    len(snap.Anchors),
		SnapshotBytes:      len(raw),
	}
	var gcable int
	switch {
	case !gcTombstones:
		result.TombstoneGCSkipped = "disabled"
	case gcReady:
		gcable, err = store.CountTombstonesBelowHLC(ctx, gcBeforeHLC)
		if err != nil {
			return err
		}
		result.TombstoneGCBeforeHLC = gcBeforeHLC
		result.TombstonesGC = gcable
	default:
		result.TombstoneGCSkipped = gcSkip
	}
	return opts.render(stdout, func(w io.Writer) error {
		if _, err := fmt.Fprintf(w, "hub compact (dry run): would publish a snapshot of %d entr(y/ies), %d tombstone(s), %d anchor(s) (~%d bytes plaintext); would delete ~%d cold event(s); keep %d snapshot(s)\n",
			len(snap.Entries), len(snap.Tombstones), len(snap.Anchors), len(raw), estimate, keepSnapshots); err != nil {
			return err
		}
		switch {
		case !gcTombstones:
			if _, err := fmt.Fprintf(w, "tombstone GC: disabled (--gc-tombstones=false)\n"); err != nil {
				return err
			}
		case gcReady:
			if _, err := fmt.Fprintf(w, "tombstone GC: would purge %d tombstone(s) below HLC %d (min ack watermark)\n", gcable, gcBeforeHLC); err != nil {
				return err
			}
		default:
			if _, err := fmt.Fprintf(w, "tombstone GC: skipped (%s)\n", gcSkip); err != nil {
				return err
			}
		}
		return printFloors(w, map[string]int64(floors))
	}, result)
}

// printFloors renders the per-device floors in deterministic device order.
func printFloors(stdout io.Writer, floors map[string]int64) error {
	devices := make([]string, 0, len(floors))
	for dev := range floors {
		devices = append(devices, dev)
	}
	sort.Strings(devices)
	for _, dev := range devices {
		if _, err := fmt.Fprintf(stdout, "  floor %s = %d\n", dev, floors[dev]); err != nil {
			return err
		}
	}
	return nil
}

// shortSHA truncates a 64-char sha256 hex for human-readable summaries.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
