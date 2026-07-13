// snapshot_build.go assembles the plaintext snapshot document from store reads
// (P4-HUB-11, producer half). It is the symmetric counterpart to
// snapshot_import.go: import derives local state from a snapshot; BuildSnapshot
// derives a snapshot from local state. V/Epoch/KID are left zero here — they are
// stamped by SealSnapshot so the document and its envelope can never disagree.
package sync

import (
	"context"

	"github.com/Reederey87/DevStrap/internal/state"
)

// BuildSnapshot reads the derived namespace map, surviving tombstones, and
// per-device chain anchors from the store and assembles a snapshot document for
// the given producer, HLC, and floors. The floors map is the exact per-device
// retention floor the compactor is about to publish; the snapshot's Floor must
// equal the manifest's Floors (recoverFromSnapshot cross-checks them), so the
// caller passes the reconciled floors and seals this document under them.
func BuildSnapshot(ctx context.Context, st *state.Store, producedBy string, hlc int64, floors Cursor) (Snapshot, error) {
	workspaceID, err := st.WorkspaceID(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	rows, err := st.SnapshotEntries(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	tombstones, err := st.SnapshotTombstones(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	anchors, err := st.ChainAnchorsForFloors(ctx, map[string]int64(floors))
	if err != nil {
		return Snapshot{}, err
	}
	trust, err := st.SnapshotTrust(ctx)
	if err != nil {
		return Snapshot{}, err
	}

	snap := Snapshot{
		WorkspaceID: workspaceID,
		ProducedBy:  producedBy,
		HLC:         hlc,
		Floor:       floors,
	}
	for _, r := range rows {
		entry := SnapshotEntry{
			Path:                  r.Path,
			PathKey:               r.PathKey,
			Type:                  r.Type,
			DisplayName:           r.DisplayName,
			MaterializationPolicy: r.MaterializationPolicy,
			Status:                r.Status,
			SourceEventHLC:        r.SourceEventHLC,
			SourceEventDeviceID:   r.SourceEventDeviceID,
			SourceEventID:         r.SourceEventID,
		}
		if r.Git != nil {
			entry.Git = &SnapshotGit{
				RemoteURL:     r.Git.RemoteURL,
				RemoteKey:     r.Git.RemoteKey,
				DefaultBranch: r.Git.DefaultBranch,
				LFSPolicy:     r.Git.LFSPolicy,
				ForgeKind:     r.Git.ForgeKind,
			}
		}
		if r.Draft != nil {
			entry.Draft = &SnapshotDraft{
				BlobRef:             r.Draft.BlobRef,
				ByteSize:            r.Draft.ByteSize,
				FileCount:           r.Draft.FileCount,
				SourceEventHLC:      r.Draft.SourceEventHLC,
				SourceEventDeviceID: r.Draft.SourceEventDeviceID,
				SourceEventID:       r.Draft.SourceEventID,
			}
		}
		if r.Env != nil {
			entry.Env = &SnapshotEnv{
				Name:                r.Env.Name,
				Provider:            r.Env.Provider,
				Mode:                r.Env.Mode,
				BlobRef:             r.Env.BlobRef,
				VarNames:            r.Env.VarNames,
				Refs:                r.Env.Refs,
				SourceEventHLC:      r.Env.SourceEventHLC,
				SourceEventDeviceID: r.Env.SourceEventDeviceID,
				SourceEventID:       r.Env.SourceEventID,
			}
		}
		snap.Entries = append(snap.Entries, entry)
	}
	for _, ts := range tombstones {
		snap.Tombstones = append(snap.Tombstones, SnapshotTombstone{
			PathKey:             ts.PathKey,
			TombstoneHLC:        ts.TombstoneHLC,
			SourceEventDeviceID: ts.SourceEventDeviceID,
			SourceEventID:       ts.SourceEventID,
		})
	}
	for _, a := range anchors {
		snap.Anchors = append(snap.Anchors, ChainAnchor{
			DeviceID:    a.DeviceID,
			Seq:         a.Seq,
			ContentHash: a.ContentHash,
			FoldedHash:  a.FoldedHash,
			HLC:         a.HLC,
		})
	}
	// Terminal device trust rides in the snapshot (P7-SYNC-01): the
	// device.revoked/device.lost events below the floor are about to be
	// deleted, so this projection is the only way a snapshot-bootstrapped
	// device ever learns the revocation.
	for _, tr := range trust {
		snap.Trust = append(snap.Trust, SnapshotTrust{
			DeviceID:     tr.DeviceID,
			State:        tr.TrustState,
			RevokedAtHLC: tr.RevokedAtHLC,
		})
	}
	return snap, nil
}
