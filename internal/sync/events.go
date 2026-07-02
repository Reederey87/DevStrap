package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/Reederey87/DevStrap/internal/id"
	"github.com/Reederey87/DevStrap/internal/logging"
	"github.com/Reederey87/DevStrap/internal/pathkey"
	"github.com/Reederey87/DevStrap/internal/redact"
	"github.com/Reederey87/DevStrap/internal/state"
)

const (
	EventProjectAdded         = "project.added"
	EventProjectUpdated       = "project.updated"
	EventProjectDeleted       = "project.deleted"
	EventProjectRenamed       = "project.renamed"
	EventConflictCreated      = "conflict.created"
	EventConflictResolved     = "conflict.resolved"      // PROD-06
	EventDraftSnapshotCreated = "draft.snapshot.created" // DRAFT-02
	EventDeviceKeyGranted     = "device.key.granted"     // P4-SEC-07: age-wrapped WCK epoch grant
)

// Conflict type identifiers, exported so the CLI resolver can branch on them
// (P5-SYNC-04) without duplicating string literals.
const (
	ConflictSamePathDifferentRemote = "same_path_different_remote"
	ConflictPendingDelete           = "pending_delete_conflict"
	ConflictRenameTargetExists      = "rename_target_exists"
	ConflictEventVerification       = "event_verification_failure"
)

// defaultReceiveMaxSkew bounds how far ahead of local physical time a remote
// event's HLC may be before it is quarantined instead of applied.
const defaultReceiveMaxSkew = 5 * time.Minute

// epochFloorMS is the minimum plausible physical timestamp. HLC values whose
// physical component is below this floor are quarantined as implausible so a
// malicious/buggy peer cannot poison ordering from the "past" direction
// (SYNC-03). Set to 0 so only truly non-positive HLC values (event.HLC <= 0)
// are rejected; deterministic tests use small positive HLC values whose
// physical component is 0. A production deployment should raise this to the
// DevStrap launch epoch once test events use realistic timestamps.
const epochFloorMS = 0

type ProjectPayload struct {
	Path          string `json:"path"`
	Type          string `json:"type"`
	RemoteURL     string `json:"remote_url,omitempty"`
	RemoteKey     string `json:"remote_key,omitempty"`
	DefaultBranch string `json:"default_branch,omitempty"`
}

// RenamePayload carries a project.renamed event's source and destination paths.
type RenamePayload struct {
	OldPath string `json:"old_path"`
	NewPath string `json:"new_path"`
}

// ConflictResolvedPayload carries a conflict.resolved event (PROD-06): the
// user's local resolution decision is audited and synced so every device sees
// the same outcome and the open-conflict count converges. ConflictID and
// NamespaceID are the origin device's local row ids (per-device, NOT stable
// across devices), retained only for display/audit; the apply handler matches
// on the stable (type, details_json) fingerprint instead (P5-SYNC-02).
type ConflictResolvedPayload struct {
	ConflictID  string `json:"conflict_id"`
	NamespaceID string `json:"namespace_id,omitempty"`
	Type        string `json:"type"`
	DetailsJSON string `json:"details_json"`
	Action      string `json:"action"` // keep-local | keep-remote | keep-both
}

// DraftSnapshotPayload carries a draft.snapshot.created event's content-addressed
// blob reference and limits (DRAFT-02).
type DraftSnapshotPayload struct {
	Path      string `json:"path"`
	BlobRef   string `json:"blob_ref"`
	ByteSize  int64  `json:"byte_size"`
	FileCount int64  `json:"file_count"`
}

// DeviceKeyGrant carries a device.key.granted event (P4-SEC-07): a Workspace
// Content Key for an epoch, age-wrapped to a single approved device's X25519
// recipient. Grant events ride the hub event log as PLAINTEXT (the decorator
// passes them through unencrypted) because their payload is already
// asymmetrically wrapped — the hub cannot decrypt the WCK without the recipient's
// private key. A newly-approved device ingests grants for every held epoch on
// its first pull so it can decrypt the entire namespace-map history.
type DeviceKeyGrant struct {
	Epoch      int64  `json:"epoch"`
	KID        string `json:"kid,omitempty"` // KIDForWCK of the wrapped key (P6-SEC-02); "" on legacy grants
	Recipient  string `json:"recipient"`     // age X25519 recipient the WCK is wrapped to
	WrappedKey string `json:"wrapped_key"`   // base64(age.Encrypt(wck, recipient))
}

type skewConflictDetails struct {
	EventID  string `json:"event_id"`
	DeviceID string `json:"device_id"`
	HLC      int64  `json:"hlc"`
}

type renameConflictDetails struct {
	OldPath  string `json:"old_path"`
	NewPath  string `json:"new_path"`
	EventID  string `json:"event_id"`
	DeviceID string `json:"device_id"`
}

type pendingDeleteConflictDetails struct {
	Path     string `json:"path"`
	EventID  string `json:"event_id"`
	DeviceID string `json:"device_id"`
	HLC      int64  `json:"hlc"`
}

type samePathCandidate struct {
	payload  ProjectPayload
	hlc      int64
	deviceID string
	eventID  string
}

type samePathConflictDetails struct {
	Path           string `json:"path"`
	RemoteKeyA     string `json:"remote_key_a"`
	RemoteKeyB     string `json:"remote_key_b"`
	WinnerKey      string `json:"winner_key"`
	WinnerHLC      int64  `json:"winner_hlc,omitempty"`
	WinnerDeviceID string `json:"winner_device_id,omitempty"`
	WinnerEventID  string `json:"winner_event_id,omitempty"`
	LoserKey       string `json:"loser_key"`
	LoserHLC       int64  `json:"loser_hlc,omitempty"`
	LoserDeviceID  string `json:"loser_device_id,omitempty"`
	LoserEventID   string `json:"loser_event_id,omitempty"`
}

type eventHashChainConflictDetails struct {
	EventID       string `json:"event_id"`
	DeviceID      string `json:"device_id"`
	HLC           int64  `json:"hlc,omitempty"`
	Seq           int64  `json:"seq,omitempty"`
	PrevEventHash string `json:"prev_event_hash"`
	Error         string `json:"error"`
}

// Machine-readable failure kinds for event_verification_failure conflicts.
// "verification" failures (signature/trust/content-hash) become applicable
// once the source device is approved, so `devices approve` replays them.
// "divergent" failures are data-integrity conflicts with an already-stored
// event of the same ID and must NEVER be auto-resolved by approval.
const (
	EventConflictKindVerification = "verification"
	EventConflictKindDivergent    = "divergent"
)

type eventVerificationConflictDetails struct {
	Kind      string `json:"kind"`
	EventID   string `json:"event_id"`
	DeviceID  string `json:"device_id"`
	HLC       int64  `json:"hlc"`
	Seq       int64  `json:"seq"`
	Type      string `json:"type"`
	Error     string `json:"error"`
	EventJSON string `json:"event_json"`
}

func NewProjectEvent(deviceID, typ string, hlc int64, payload ProjectPayload) (state.Event, error) {
	eventID, err := id.New("evt")
	if err != nil {
		return state.Event{}, err
	}
	payload.RemoteURL = redact.StripURLUserinfo(payload.RemoteURL)
	raw, err := json.Marshal(payload)
	if err != nil {
		return state.Event{}, err
	}
	return state.Event{
		ID:          eventID,
		DeviceID:    deviceID,
		HLC:         hlc,
		Type:        typ,
		PayloadJSON: string(raw),
		ContentHash: state.ContentHash(string(raw)),
	}, nil
}

func CreateProjectEvent(ctx context.Context, st *state.Store, typ string, payload ProjectPayload) (state.Event, error) {
	payload.RemoteURL = redact.StripURLUserinfo(payload.RemoteURL)
	raw, err := json.Marshal(payload)
	if err != nil {
		return state.Event{}, err
	}
	return st.InsertLocalEvent(ctx, state.Event{
		Type:        typ,
		PayloadJSON: string(raw),
		ContentHash: state.ContentHash(string(raw)),
	})
}

// NewDraftSnapshotEvent builds an unsigned draft.snapshot.created event from a
// pre-marshaled payload (DRAFT-02). The store stamps HLC, seq, device id, and
// the device signature on InsertLocalEvent.
func NewDraftSnapshotEvent(typ, payloadJSON string) state.Event {
	return state.Event{
		Type:        typ,
		PayloadJSON: payloadJSON,
		ContentHash: state.ContentHash(payloadJSON),
	}
}

// NewDeviceKeyGrantEvent builds an unsigned device.key.granted event from a
// pre-marshaled DeviceKeyGrant payload (P4-SEC-07). The store stamps HLC, seq,
// device id, and the device signature on InsertLocalEvent. Grant events are NOT
// envelope-encrypted (the payload is itself age-wrapped), so the EncryptedHub
// decorator passes them through unchanged on both Push and Pull.
func NewDeviceKeyGrantEvent(typ, payloadJSON string) state.Event {
	return state.Event{
		Type:        typ,
		PayloadJSON: payloadJSON,
		ContentHash: state.ContentHash(payloadJSON),
	}
}

// CreateConflictResolvedEvent builds and inserts a conflict.resolved event
// (PROD-06) recording the user's resolution decision so it syncs to every
// device and the open-conflict count converges.
func CreateConflictResolvedEvent(ctx context.Context, st *state.Store, payload ConflictResolvedPayload) (state.Event, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return state.Event{}, err
	}
	return st.InsertLocalEvent(ctx, state.Event{
		Type:        EventConflictResolved,
		PayloadJSON: string(raw),
		ContentHash: state.ContentHash(string(raw)),
	})
}

// ApplyEvents sorts and applies a batch of remote events. It returns
// safeAdvanceHLC: the highest HLC value that is safe to advance the hub pull
// cursor to (SYNC-01 low-water mark). The cursor must never advance past an
// event that was skipped (quarantined or hash-chain-broken) within this batch,
// otherwise that event is never re-delivered and the gap is permanent.
//
// safeAdvanceHLC = min(maxAppliedHLC, lowestUnappliedHLC-1). When no event was
// skipped, lowestUnappliedHLC is +Inf and safeAdvanceHLC equals maxAppliedHLC.
//
// Only TRANSIENTLY-skipped events hold back the cursor: skew-ahead quarantine
// (a remote clock a few minutes ahead; it becomes valid once local time
// catches up) and hash-chain breaks (a re-delivery may eventually carry the
// correct prev_event_hash). Permanently-invalid events (HLC <= 0 or below the
// epoch floor), verification failures (signature/trust/content-hash), and
// divergent duplicate event IDs are recorded as conflicts and do NOT hold the
// cursor, since re-delivery would fail identically forever and would wedge sync.
func ApplyEvents(ctx context.Context, st *state.Store, events []state.Event) (int64, error) {
	sort.Slice(events, func(i, j int) bool {
		if events[i].HLC == events[j].HLC {
			if events[i].DeviceID == events[j].DeviceID {
				return events[i].ID < events[j].ID
			}
			return events[i].DeviceID < events[j].DeviceID
		}
		return events[i].HLC < events[j].HLC
	})
	now := time.Now().UnixMilli()
	maxSkewMS := defaultReceiveMaxSkew.Milliseconds()
	var maxAppliedHLC int64
	lowestUnapplied := int64(math.MaxInt64)
	for _, event := range events {
		// SYNC-03: quarantine remote events with implausible HLC values
		// (non-positive or below the epoch floor) so they cannot win every
		// same-path conflict from the "past" direction. These are permanently
		// invalid and do NOT hold back the cursor (SYNC-01).
		physical := event.HLC >> hlcLogicalBits
		if event.HLC <= 0 || physical < epochFloorMS {
			if err := quarantineSkewedEvent(ctx, st, event, physical-now); err != nil {
				return 0, err
			}
			continue
		}
		// SYNC-3: quarantine remote events whose physical timestamp is beyond
		// the trusted skew so one bad/malicious peer cannot poison ordering.
		// Skipping (not aborting) keeps the rest of the batch converging. This
		// is TRANSIENT (bounded by maxSkew) so it holds back the cursor
		// (SYNC-01) until local time catches up and the event is re-delivered.
		if offset := physical - now; offset > maxSkewMS {
			if err := quarantineSkewedEvent(ctx, st, event, offset); err != nil {
				return 0, err
			}
			if event.HLC < lowestUnapplied {
				lowestUnapplied = event.HLC
			}
			continue
		}
		var inserted bool
		if err := st.WithTx(ctx, func(tx *state.Tx) error {
			// Re-stamp the workspace_id with the local workspace so the events
			// FK constraint is satisfied. Remote events carry the origin
			// device's workspace_id; each device has its own workspace row but
			// shares the same logical namespace. The signature payload does
			// not include workspace_id, so this does not invalidate it.
			event.WorkspaceID = ""
			// Ensure the source device exists locally as a placeholder so the
			// events FK constraint is satisfied. Remote devices appear as
			// 'pending' until enrolled; their events are accepted during the
			// bootstrap window (HUB-03).
			if err := tx.EnsureRemoteDeviceTx(ctx, event.DeviceID); err != nil {
				return err
			}
			var err error
			inserted, err = tx.InsertEvent(ctx, event)
			if err != nil {
				return err
			}
			if !inserted {
				return nil
			}
			if err := tx.ReceiveRemoteHLC(ctx, event.HLC); err != nil {
				return err
			}
			return applyEventTx(ctx, tx, event)
		}); err != nil {
			if errors.Is(err, state.ErrEventHashChain) {
				if conflictErr := insertEventHashChainConflict(ctx, st, event, err); conflictErr != nil {
					return 0, errors.Join(err, conflictErr)
				}
				// SYNC-05/CODE-01: record the conflict and continue so the
				// rest of the batch (and other devices' events) still apply,
				// mirroring the skew-quarantine path. The broken event is
				// never inserted. SYNC-01: hold the cursor below it so it is
				// re-delivered next pull (a re-delivery may carry the correct
				// prev_event_hash); insertConflict dedups on stable details so
				// no unbounded growth results.
				if event.HLC < lowestUnapplied {
					lowestUnapplied = event.HLC
				}
				continue
			}
			if errors.Is(err, state.ErrEventVerification) || errors.Is(err, state.ErrDivergentEvent) {
				if conflictErr := insertEventVerificationConflict(ctx, st, event, err); conflictErr != nil {
					return 0, errors.Join(err, conflictErr)
				}
				// Permanent verification/divergence failures are quarantined
				// and counted as consumed for the cursor: re-delivery would
				// fail identically forever (the full event is preserved in the
				// conflict for approval replay), and without advancing here a
				// batch ENDING in a quarantined event would be re-delivered by
				// the inclusive pull boundary on every subsequent sync.
				if event.HLC > maxAppliedHLC {
					maxAppliedHLC = event.HLC
				}
				continue
			}
			return 0, err
		}
		if inserted && event.HLC > maxAppliedHLC {
			maxAppliedHLC = event.HLC
		}
	}
	// SYNC-01: advance the cursor only to the low-water mark — never past a
	// transiently-skipped event — so skipped events are re-delivered next pull
	// instead of being permanently stranded.
	safe := maxAppliedHLC
	if lowestUnapplied != math.MaxInt64 && lowestUnapplied-1 < safe {
		safe = lowestUnapplied - 1
	}
	if lowestUnapplied != math.MaxInt64 {
		logging.Logger(ctx).Warn("sync cursor held back by unapplied event",
			"lowest_unapplied_hlc", lowestUnapplied, "safe_advance_hlc", safe, "max_applied_hlc", maxAppliedHLC)
	}
	return safe, nil
}

func quarantineSkewedEvent(ctx context.Context, st *state.Store, event state.Event, offsetMS int64) error {
	logging.Logger(ctx).Warn("quarantined remote event with untrustworthy time",
		"device_id", event.DeviceID, "event_id", event.ID, "offset_ms", offsetMS)
	// CODE-02: omit the volatile offset_ms from the persisted details so
	// insertConflict dedups re-delivered skewed events instead of inserting
	// a new conflict row on every resync.
	raw, err := json.Marshal(skewConflictDetails{
		EventID:  event.ID,
		DeviceID: event.DeviceID,
		HLC:      event.HLC,
	})
	if err != nil {
		return err
	}
	return st.InsertConflict(ctx, "", "untrustworthy_remote_time", string(raw))
}

func insertEventHashChainConflict(ctx context.Context, st *state.Store, event state.Event, cause error) error {
	raw, err := json.Marshal(eventHashChainConflictDetails{
		EventID:       event.ID,
		DeviceID:      event.DeviceID,
		HLC:           event.HLC,
		Seq:           event.Seq,
		PrevEventHash: event.PrevEventHash,
		Error:         cause.Error(),
	})
	if err != nil {
		return err
	}
	return st.InsertConflict(ctx, "", "event_hash_chain_break", string(raw))
}

func insertEventVerificationConflict(ctx context.Context, st *state.Store, event state.Event, cause error) error {
	// Dedup on event ID, not exact details: the same event re-quarantined for
	// a different reason (e.g. an approve-time replay that fails the signature
	// check where the original failure was pending trust) must not open a
	// second conflict row — the error string is volatile, the event is not.
	existing, err := st.OpenConflictsByType(ctx, ConflictEventVerification)
	if err != nil {
		return err
	}
	for _, c := range existing {
		var d eventVerificationConflictDetails
		if json.Unmarshal([]byte(c.DetailsJSON), &d) == nil && d.EventID == event.ID {
			return nil
		}
	}
	eventRaw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	kind := EventConflictKindVerification
	if errors.Is(cause, state.ErrDivergentEvent) {
		kind = EventConflictKindDivergent
	}
	raw, err := json.Marshal(eventVerificationConflictDetails{
		Kind:      kind,
		EventID:   event.ID,
		DeviceID:  event.DeviceID,
		HLC:       event.HLC,
		Seq:       event.Seq,
		Type:      event.Type,
		Error:     cause.Error(),
		EventJSON: string(eventRaw),
	})
	if err != nil {
		return err
	}
	return st.InsertConflict(ctx, "", ConflictEventVerification, string(raw))
}

func applyEventTx(ctx context.Context, tx *state.Tx, event state.Event) error {
	switch event.Type {
	case EventProjectAdded, EventProjectUpdated:
		var payload ProjectPayload
		if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err != nil {
			return fmt.Errorf("decode event %s: %w", event.ID, err)
		}
		if tombstoneHLC, ok, err := tx.TombstoneHLC(ctx, payload.Path); err != nil {
			return err
		} else if ok && event.HLC <= tombstoneHLC {
			return nil
		}
		existing, err := tx.ProjectByPath(ctx, payload.Path)
		if err == nil && existing.RemoteKey != "" && payload.RemoteKey != "" && existing.RemoteKey != payload.RemoteKey {
			winner, incomingWins, details, err := reconcileSamePath(existing, payload, event)
			if err != nil {
				return err
			}
			if incomingWins {
				if _, err := tx.UpsertProject(ctx, upsertParamsForEvent(winner, event)); err != nil {
					return err
				}
			}
			return tx.InsertConflict(ctx, existing.ID, ConflictSamePathDifferentRemote, details)
		}
		// SYNC-01: same-remote add/update must be HLC last-writer-wins. Only
		// mutate when the incoming event coordinates strictly dominate the
		// stored source-event coordinates; otherwise no-op so convergence is
		// deterministic regardless of arrival order.
		if err == nil {
			cur := samePathCandidate{hlc: existing.SourceEventHLC, deviceID: existing.SourceEventDeviceID, eventID: existing.SourceEventID}
			inc := samePathCandidate{hlc: event.HLC, deviceID: event.DeviceID, eventID: event.ID}
			if !samePathLess(cur, inc) {
				return nil // stored coords dominate → stale, skip
			}
		}
		_, err = tx.UpsertProject(ctx, upsertParamsForEvent(payload, event))
		return err
	case EventProjectDeleted:
		var payload ProjectPayload
		if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err != nil {
			return fmt.Errorf("decode event %s: %w", event.ID, err)
		}
		// SYNC-5: never destroy a dirty local checkout on a remote delete;
		// surface a conflict for the user to resolve instead.
		if existing, err := tx.ProjectByPath(ctx, payload.Path); err == nil && existing.DirtyState == dirtyStateDirty {
			raw, err := json.Marshal(pendingDeleteConflictDetails{
				Path:     payload.Path,
				EventID:  event.ID,
				DeviceID: event.DeviceID,
				HLC:      event.HLC,
			})
			if err != nil {
				return err
			}
			return tx.InsertConflict(ctx, existing.ID, ConflictPendingDelete, string(raw))
		}
		return tx.TombstoneProject(ctx, payload.Path, event.HLC)
	case EventProjectRenamed:
		var payload RenamePayload
		if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err != nil {
			return fmt.Errorf("decode event %s: %w", event.ID, err)
		}
		outcome, err := tx.RenameProject(ctx, payload.OldPath, payload.NewPath, event)
		if err != nil {
			return err
		}
		if outcome == state.RenameTargetConflict {
			raw, err := json.Marshal(renameConflictDetails{
				OldPath:  payload.OldPath,
				NewPath:  payload.NewPath,
				EventID:  event.ID,
				DeviceID: event.DeviceID,
			})
			if err != nil {
				return err
			}
			return tx.InsertConflict(ctx, "", ConflictRenameTargetExists, string(raw))
		}
		return nil
	case EventConflictCreated:
		return tx.InsertConflict(ctx, "", "remote_conflict", event.PayloadJSON)
	case EventConflictResolved:
		// PROD-06: mark the matching open conflict resolved on the receiving
		// device so the open-conflict count converges. Conflict IDs are
		// per-device, so match on the stable (namespace_id, type, details_json)
		// fingerprint. The UPDATE is idempotent (WHERE status='open'): a
		// duplicate event for an already-resolved row is a no-op.
		var payload ConflictResolvedPayload
		if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err != nil {
			return fmt.Errorf("decode event %s: %w", event.ID, err)
		}
		resolution, _ := json.Marshal(map[string]string{
			"action":       payload.Action,
			"resolved_by":  event.DeviceID,
			"resolved_hlc": fmt.Sprintf("%d", event.HLC),
		})
		// P5-SYNC-02: match on (type, details_json) only — namespace_id is
		// per-device and would never match on the receiving device.
		return tx.ResolveConflictByFingerprint(ctx, payload.Type, payload.DetailsJSON, string(resolution))
	case EventDraftSnapshotCreated:
		var payload DraftSnapshotPayload
		if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err != nil {
			return fmt.Errorf("decode event %s: %w", event.ID, err)
		}
		// DRAFT-02: record the content-addressed blob reference against the
		// project. The blob content is fetched from the hub during sync and
		// extracted during materialization.
		pk, err := pathkey.Clean(payload.Path)
		if err != nil {
			return fmt.Errorf("draft snapshot path %q: %w", payload.Path, err)
		}
		project, err := tx.ProjectByPath(ctx, pk.Display)
		if err != nil {
			return fmt.Errorf("draft snapshot for unknown project %q: %w", payload.Path, err)
		}
		return tx.RecordDraftSnapshotTx(ctx, project.ID, payload.BlobRef, payload.ByteSize, payload.FileCount, event)
	case EventDeviceKeyGranted:
		// P4-SEC-07: record the grant audit row transactionally with the event
		// insert. The secret WCK is ingested into the keychain by the
		// EncryptedHub decorator during Pull (on the recipient device only);
		// this case only records the non-secret membership audit on every
		// device that applies the grant. Grant events are intentionally NOT in
		// mustVerifyEvent so a newly-approved device can ingest its first WCK
		// during the pre-enrollment bootstrap window (SEC-04); they inherit
		// SEC-04 fail-closed trust once that lands.
		var payload DeviceKeyGrant
		if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err != nil {
			return fmt.Errorf("decode event %s: %w", event.ID, err)
		}
		return tx.RecordKeyGrantTx(ctx, payload.Epoch, payload.KID, payload.Recipient, event)
	default:
		return nil
	}
}

// dirtyStateDirty mirrors git.DirtyDirty without importing the git package into
// the sync layer.
const dirtyStateDirty = "dirty"

func upsertParamsForEvent(payload ProjectPayload, event state.Event) state.UpsertProjectParams {
	return state.UpsertProjectParams{
		Path:                  payload.Path,
		Type:                  payload.Type,
		RemoteURL:             payload.RemoteURL,
		RemoteKey:             payload.RemoteKey,
		DefaultBranch:         payload.DefaultBranch,
		MaterializationPolicy: "lazy",
		MaterializationState:  "skeleton",
		SourceEventHLC:        event.HLC,
		SourceEventDeviceID:   event.DeviceID,
		SourceEventID:         event.ID,
	}
}

func reconcileSamePath(existing state.ProjectStatus, incoming ProjectPayload, event state.Event) (ProjectPayload, bool, string, error) {
	current := samePathCandidate{
		payload: ProjectPayload{
			Path:          existing.Path,
			Type:          existing.Type,
			RemoteURL:     existing.RemoteURL,
			RemoteKey:     existing.RemoteKey,
			DefaultBranch: existing.DefaultBranch,
		},
		hlc:      existing.SourceEventHLC,
		deviceID: existing.SourceEventDeviceID,
		eventID:  existing.SourceEventID,
	}
	next := samePathCandidate{
		payload:  incoming,
		hlc:      event.HLC,
		deviceID: event.DeviceID,
		eventID:  event.ID,
	}
	winner, loser, incomingWins := current, next, false
	if samePathLess(next, current) {
		winner, loser, incomingWins = next, current, true
	}
	remoteA, remoteB := current.payload.RemoteKey, next.payload.RemoteKey
	if remoteB < remoteA {
		remoteA, remoteB = remoteB, remoteA
	}
	raw, err := json.Marshal(samePathConflictDetails{
		Path:           incoming.Path,
		RemoteKeyA:     remoteA,
		RemoteKeyB:     remoteB,
		WinnerKey:      winner.payload.RemoteKey,
		WinnerHLC:      winner.hlc,
		WinnerDeviceID: winner.deviceID,
		WinnerEventID:  winner.eventID,
		LoserKey:       loser.payload.RemoteKey,
		LoserHLC:       loser.hlc,
		LoserDeviceID:  loser.deviceID,
		LoserEventID:   loser.eventID,
	})
	if err != nil {
		return ProjectPayload{}, false, "", err
	}
	return winner.payload, incomingWins, string(raw), nil
}

// SamePathConflictInfo is the exported, parsed view of a
// same_path_different_remote conflict's details (P5-SYNC-04), so the CLI
// resolver can recover the competing variants and their origin events.
type SamePathConflictInfo struct {
	Path          string
	WinnerKey     string
	WinnerEventID string
	LoserKey      string
	LoserEventID  string
}

// ParseSamePathConflictDetails decodes a same_path_different_remote conflict's
// details_json (P5-SYNC-04).
func ParseSamePathConflictDetails(detailsJSON string) (SamePathConflictInfo, error) {
	var d samePathConflictDetails
	if err := json.Unmarshal([]byte(detailsJSON), &d); err != nil {
		return SamePathConflictInfo{}, fmt.Errorf("decode same-path conflict details: %w", err)
	}
	return SamePathConflictInfo{
		Path:          d.Path,
		WinnerKey:     d.WinnerKey,
		WinnerEventID: d.WinnerEventID,
		LoserKey:      d.LoserKey,
		LoserEventID:  d.LoserEventID,
	}, nil
}

// ParsePendingDeleteConflictPath decodes a pending_delete_conflict's path
// (P5-SYNC-04).
func ParsePendingDeleteConflictPath(detailsJSON string) (string, error) {
	var d pendingDeleteConflictDetails
	if err := json.Unmarshal([]byte(detailsJSON), &d); err != nil {
		return "", fmt.Errorf("decode pending-delete conflict details: %w", err)
	}
	return d.Path, nil
}

// ProjectPayloadFromEvent decodes a project event's payload (P5-SYNC-04), used
// to recover a losing variant's full remote.
func ProjectPayloadFromEvent(payloadJSON string) (ProjectPayload, error) {
	var p ProjectPayload
	if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
		return ProjectPayload{}, fmt.Errorf("decode project payload: %w", err)
	}
	return p, nil
}

func samePathLess(a, b samePathCandidate) bool {
	if a.hlc != b.hlc {
		return a.hlc < b.hlc
	}
	if a.deviceID != b.deviceID {
		return a.deviceID < b.deviceID
	}
	return a.eventID < b.eventID
}
