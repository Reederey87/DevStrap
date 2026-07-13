package sync

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Reederey87/DevStrap/internal/logging"
	"github.com/Reederey87/DevStrap/internal/state"
)

// Omission-conflict kinds (P4-SYNC-05).
const (
	// OmissionKindWithheldTail: a peer's signed head commits to a seq beyond the
	// contiguous prefix we received, and the shortfall persisted across two
	// cycles (a one-cycle grace absorbs the in-flight race). The hub is
	// withholding that peer's newest events.
	OmissionKindWithheldTail = "withheld_tail"
	// OmissionKindFork: our independently-folded prefix disagrees with the
	// peer's signed fold at the same seq — the peer equivocated (two different
	// signed heads at one seq) or the hub spliced the stream. An integrity
	// failure, not merely an availability one.
	OmissionKindFork = "fork"
)

// omissionConflictDetails is the stable, dedup-friendly detail body for an
// event_omission conflict. It deliberately OMITS the promised seq: the promise a
// live peer publishes grows with every new ack, so keying the conflict on it
// would defeat InsertConflict's identical-details_json dedup and accumulate one
// open row per cycle for an honest, actively-syncing peer. The dedup identity is
// the stable (device_id, kind, local_seq) — for a fixed withheld tail local_seq
// (the seq our contiguous prefix stops at) does not move, so re-alarms collapse
// to one row.
type omissionConflictDetails struct {
	DeviceID string `json:"device_id"`
	Kind     string `json:"kind"`
	LocalSeq int64  `json:"local_seq"`
}

// localDeclineConflictTypes are the quarantine-class conflicts that mean THIS
// device declined/quarantined a peer's event (leaving a fold gap) rather than
// the hub withholding it (P4-SYNC-05 defect fix). event_omission itself is
// excluded — it is the alarm we are deciding whether to raise, not evidence of a
// local decline.
var localDeclineConflictTypes = []string{
	ConflictUntrustworthyTime,
	ConflictEventHashChain,
	ConflictEventVerification,
}

// VerifyPeerHeads is the pull-path omission/fork detector (P4-SYNC-05). For each
// APPROVED peer that has published a signed head (its ack marker's FoldedHash),
// it compares the head's committed (seq, fold) against the contiguous prefix
// this device actually holds and folds independently:
//
//   - the head commits to a seq beyond our contiguous prefix, and that shortfall
//     persists across two cycles → OmissionKindWithheldTail (the hub is
//     withholding that peer's newest events);
//   - our folded prefix disagrees with the peer's signed fold at a seq we DO
//     hold, or the peer signs two different heads at the same seq →
//     OmissionKindFork.
//
// A single cycle short of the head is treated as the legitimate in-flight race
// (a peer pulled between an origin's event push and its ack) and only arms a
// one-cycle grace. The per-peer watermark (state.DeviceHead) persists that grace
// and the highest promise ever verified, so a hub cannot retract a promise by
// serving a stale ack.
//
// Returns the number of conflicts recorded this pass. Hub/store errors are
// returned to the caller; a peer whose ack is unverifiable, unparseable, or v1
// (no fold) is skipped fail-safe (never a false alarm).
func VerifyPeerHeads(ctx context.Context, st *state.Store, hub Hub, workspaceID, selfDeviceID string) (int, error) {
	rawAcks, err := hub.ListAcks(ctx)
	if err != nil {
		return 0, fmt.Errorf("list sync acks for head verification: %w", err)
	}
	log := logging.Logger(ctx)
	detected := 0
	for dev, raw := range rawAcks {
		if dev == selfDeviceID {
			continue // our own head; nothing to check against
		}
		pub, approved, aerr := st.ApprovedDeviceSigningKey(ctx, dev)
		if aerr != nil {
			return detected, aerr
		}
		if !approved {
			continue // revoked/lost/pending/unknown — its head carries no authority
		}
		m, perr := ParseAckMarker(raw)
		if perr != nil {
			continue // unparseable — its owner overwrites it next sync
		}
		if m.DeviceID != dev || m.WorkspaceID != workspaceID {
			continue // key/payload device mismatch or wrong workspace
		}
		if verr := VerifyAckMarker(m, pub); verr != nil {
			continue // bad signature
		}
		if m.FoldedHash == "" {
			continue // v1 marker or no fold seed on the producer — no head to check
		}

		conflict, err := verifyOnePeerHead(ctx, st, dev, m)
		if err != nil {
			return detected, err
		}
		if conflict != "" {
			detected++
			log.Warn("omission/fork detected against peer signed head",
				"device_id", dev, "kind", conflict, "promised_seq", m.PushedThroughSeq)
		}
	}
	return detected, nil
}

// verifyOnePeerHead evaluates one verified peer head against the local stream
// and records a conflict when warranted. It returns the conflict kind recorded
// (or "" when none), advancing the persisted per-peer watermark either way.
func verifyOnePeerHead(ctx context.Context, st *state.Store, dev string, m AckMarker) (string, error) {
	headSeq := m.PushedThroughSeq
	headFold := m.FoldedHash

	prev, _, err := st.DeviceHead(ctx, dev)
	if err != nil {
		return "", err
	}
	prev.DeviceID = dev
	next := prev

	// Equivocation: the peer already committed a DIFFERENT fold at this exact
	// seq. Two distinct signed heads at one seq is a fork regardless of what we
	// hold locally.
	if prev.PromisedSeq > 0 && headSeq == prev.PromisedSeq && headFold != prev.PromisedFold {
		if err := recordOmission(ctx, st, dev, OmissionKindFork, prev.PromisedSeq); err != nil {
			return "", err
		}
		return OmissionKindFork, nil
	}

	// Advance the promise monotonically — a hub serving a stale (lower-seq) ack
	// must never retract a promise we already recorded.
	if headSeq > next.PromisedSeq {
		next.PromisedSeq = headSeq
		next.PromisedFold = headFold
		next.PromisedHLC = m.ProducedAt
	}

	// Fold our contiguous prefix up to the promised seq.
	localSeq, localFold, seeded, err := st.DeviceFold(ctx, dev, next.PromisedSeq)
	if err != nil {
		return "", err
	}
	if !seeded {
		// No trustworthy fold seed (compacted below the floor without an anchor
		// fold, or the stream does not start at seq 1). Cannot verify; record the
		// advanced promise and clear the grace so a later seeded fold can detect.
		next.OmissionPending = false
		return "", st.UpsertDeviceHead(ctx, next)
	}

	if localSeq >= next.PromisedSeq {
		// We hold the promised prefix; the fold MUST match at that seq.
		if localFold != next.PromisedFold {
			next.OmissionPending = false
			if err := st.UpsertDeviceHead(ctx, next); err != nil {
				return "", err
			}
			if err := recordOmission(ctx, st, dev, OmissionKindFork, localSeq); err != nil {
				return "", err
			}
			return OmissionKindFork, nil
		}
		// Consistent and fully caught up: our independently-computed fold equals
		// the peer's signed fold at its promised seq — provable completeness. Clear
		// the grace and resolve any WITHHELD_TAIL raised for this peer earlier, so a
		// backfilled tail stops blocking `hub gc`. A `fork` is deliberately NOT
		// auto-resolved here: it is a durable integrity signal (we once held a
		// prefix that disagreed with the peer's signed fold) and stays open for
		// operator attention / `conflicts resolve`.
		next.OmissionPending = false
		if err := st.UpsertDeviceHead(ctx, next); err != nil {
			return "", err
		}
		return "", resolveOmission(ctx, st, dev, OmissionKindWithheldTail, "peer fold caught up to promised head")
	}

	// localSeq < promisedSeq: we are missing the tail the head commits to. Before
	// blaming the hub, check whether the first missing slot is one THIS device
	// declined/deferred/quarantined (a cross-epoch key-grant grace, a
	// consumed-quarantine skew/hash-chain/verification gap). Such a gap is a LOCAL
	// condition, not hub withholding: it is separately surfaced (and, for real
	// quarantines, already blocks gc), so raising a withheld_tail here would be a
	// false omission alarm against an honest hub. Suppress it, and resolve any
	// stale withheld_tail we raised before the local decline was recorded.
	gapSeq := localSeq + 1
	declined, err := st.DeviceGapLocallyDeclined(ctx, dev, gapSeq, localDeclineConflictTypes)
	if err != nil {
		return "", err
	}
	if declined {
		next.OmissionPending = false
		if err := st.UpsertDeviceHead(ctx, next); err != nil {
			return "", err
		}
		return "", resolveOmission(ctx, st, dev, OmissionKindWithheldTail, "gap is a local decline, not hub withholding")
	}

	// Alarm only if we were ALREADY pending AND have not even reached the
	// PREVIOUS promise (the earlier-promised events, pushed before that earlier
	// ack, still have not arrived — a persistent withhold, not an in-flight
	// race). Reaching the previous promise but trailing a newer one is
	// legitimate one-cycle lag on a growing stream, so it renews the grace.
	caughtUpToOldPromise := prev.PromisedSeq > 0 && localSeq >= prev.PromisedSeq
	if prev.OmissionPending && !caughtUpToOldPromise {
		next.OmissionPending = true
		if err := st.UpsertDeviceHead(ctx, next); err != nil {
			return "", err
		}
		if err := recordOmission(ctx, st, dev, OmissionKindWithheldTail, localSeq); err != nil {
			return "", err
		}
		return OmissionKindWithheldTail, nil
	}
	// First observation (or just caught up to the old promise): arm the grace.
	next.OmissionPending = true
	return "", st.UpsertDeviceHead(ctx, next)
}

func recordOmission(ctx context.Context, st *state.Store, dev, kind string, localSeq int64) error {
	raw, err := json.Marshal(omissionConflictDetails{
		DeviceID: dev,
		Kind:     kind,
		LocalSeq: localSeq,
	})
	if err != nil {
		return err
	}
	return st.InsertConflict(ctx, "", ConflictEventOmission, string(raw))
}

// resolveOmission clears open event_omission conflicts of the given kind for a
// peer once the underlying condition has cleared — the peer's fold caught up to
// its promised head (provable consistency), or a shortfall was reclassified as a
// local decline. An empty kind resolves every omission conflict for the device.
// Idempotent: a no-op when none are open. Without this, the alarm — re-created on
// every pull by VerifyPeerHeads while the gap persists — would never clear, and
// (since it blocks `hub gc`) would permanently wedge reclamation even after the
// gap was backfilled (P4-SYNC-05 defect fix).
func resolveOmission(ctx context.Context, st *state.Store, dev, kind, reason string) error {
	resolution := fmt.Sprintf(`{"action":"auto","reason":%q}`, reason)
	return st.ResolveOmissionConflictsForDevice(ctx, ConflictEventOmission, dev, kind, resolution)
}

// ParseOmissionConflictDetails decodes an event_omission conflict's details for
// display/audit.
func ParseOmissionConflictDetails(detailsJSON string) (deviceID, kind string, localSeq int64, err error) {
	var d omissionConflictDetails
	if uerr := json.Unmarshal([]byte(detailsJSON), &d); uerr != nil {
		return "", "", 0, fmt.Errorf("decode omission conflict details: %w", uerr)
	}
	return d.DeviceID, d.Kind, d.LocalSeq, nil
}
