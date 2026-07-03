package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	ConflictUntrustworthyTime       = "untrustworthy_remote_time"
	ConflictEventHashChain          = "event_hash_chain_break"
)

// QuarantineConflictTypes are the conflict types that mean a pulled event was
// NOT applied (skew quarantine, hash-chain break, verification failure). While
// any such conflict is open, the local replica's derived state may be missing
// references other devices still rely on, so mark-and-sweep consumers
// (hub gc, P6-HUB-01) must refuse to sweep.
var QuarantineConflictTypes = []string{
	ConflictUntrustworthyTime,
	ConflictEventHashChain,
	ConflictEventVerification,
}

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
// "undecryptable" failures are enc.v2 carriers that failed AEAD
// authentication on every held key (corruption, forgery, or a hub-side
// carrier mutation — P6-SYNC-04): permanent, never applied, and never
// replayed by approval (a replay would fail authentication identically).
const (
	EventConflictKindVerification  = "verification"
	EventConflictKindDivergent     = "divergent"
	EventConflictKindUndecryptable = "undecryptable"
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
	return createProjectEvent(ctx, st, nil, typ, payload)
}

func CreateProjectEventTx(ctx context.Context, st *state.Store, tx *state.Tx, typ string, payload ProjectPayload) (state.Event, error) {
	return createProjectEvent(ctx, st, tx, typ, payload)
}

func createProjectEvent(ctx context.Context, st *state.Store, tx *state.Tx, typ string, payload ProjectPayload) (state.Event, error) {
	payload.RemoteURL = redact.StripURLUserinfo(payload.RemoteURL)
	raw, err := json.Marshal(payload)
	if err != nil {
		return state.Event{}, err
	}
	event := state.Event{
		Type:        typ,
		PayloadJSON: string(raw),
		ContentHash: state.ContentHash(string(raw)),
	}
	if tx != nil {
		return st.InsertLocalEventTx(ctx, tx, event)
	}
	return st.InsertLocalEvent(ctx, event)
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
	return createConflictResolvedEvent(ctx, st, nil, payload)
}

func CreateConflictResolvedEventTx(ctx context.Context, st *state.Store, tx *state.Tx, payload ConflictResolvedPayload) (state.Event, error) {
	return createConflictResolvedEvent(ctx, st, tx, payload)
}

func createConflictResolvedEvent(ctx context.Context, st *state.Store, tx *state.Tx, payload ConflictResolvedPayload) (state.Event, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return state.Event{}, err
	}
	event := state.Event{
		Type:        EventConflictResolved,
		PayloadJSON: string(raw),
		ContentHash: state.ContentHash(string(raw)),
	}
	if tx != nil {
		return st.InsertLocalEventTx(ctx, tx, event)
	}
	return st.InsertLocalEvent(ctx, event)
}

// ApplyEvents sorts and applies a batch of remote events. It returns the safe
// per-device transport cursor (P5-SYNC-01): for each origin device, the
// highest Seq such that every slot from after[dev]+1 up to it was CONSUMED by
// this batch. The cursor must never advance past an event that was transiently
// skipped within this batch, otherwise that event is never re-delivered and
// the gap is permanent.
//
// Per origin device, the safe cursor is the end of the contiguous consumed run
// starting at after[dev]+1. An event is CONSUMED when it was applied, deduped
// (already inserted — deduplication is consumption, so a device re-pulling its
// own events advances past them instead of re-fetching forever), or
// permanently quarantined (implausible HLC, verification/divergence failure,
// undecryptable enc.v2 carrier): re-delivery of those would fail identically
// forever. Only TRANSIENTLY-skipped events HOLD a device's cursor: skew-ahead
// quarantine (valid once local time catches up) and hash-chain breaks (a
// re-delivery may carry the correct prev_event_hash). The hold is scoped to
// the offending origin device — other devices' cursors keep advancing
// (per-device fault isolation the old global HLC low-water mark never had). A
// Seq gap in the batch (an object missing from the hub) also stops the run,
// loudly: advancing over it would permanently strand the missing event.
//
// Events with Seq <= 0 (pre-sequence legacy) apply or quarantine as normal but
// never touch the cursor; they are re-delivered each pull and dedup by ID.
func ApplyEvents(ctx context.Context, st *state.Store, events []state.Event) (Cursor, error) {
	safe, _, err := ApplyEventsWithStats(ctx, st, events, nil)
	return safe, err
}

// ApplyStats reports what a single ApplyEvents batch did NOT apply. A non-zero
// Quarantined or a true CursorHeld means the local replica's derived state may
// be missing references other devices still rely on, so mark-and-sweep
// consumers (hub gc, P6-HUB-01) must refuse to sweep this cycle.
type ApplyStats struct {
	// Quarantined counts events recorded as conflicts instead of applied
	// (skew quarantine, hash-chain break, verification/divergence failure).
	Quarantined int
	// CursorHeld is true when at least one device's safe cursor stopped short
	// of that device's highest batch Seq — either a transiently-held event
	// (skew/hash-chain, re-delivered next pull) or a Seq gap on the hub.
	CursorHeld bool
}

// seqOutcome tracks how the events at one (device, seq) slot were disposed.
// held dominates consumed: a hub can serve a forged duplicate carrier at a
// real event's slot (the carrier fields of an undecryptable envelope are
// unauthenticated), and a consumed forgery must never advance the cursor past
// the real, transiently-held occupant of the same slot — but that guard only
// covers slots whose real occupant is IN the batch. A byzantine hub that
// WITHHOLDS the real event at a slot and serves only a forged held-epoch
// carrier there gets the slot consumed (quarantined), advancing the cursor
// past the withheld event permanently — a documented residual of the
// untrusted-hub availability model (spec/15): it is loud (a durable
// undecryptable conflict plus the successor's hash-chain hold), integrity
// still holds, and the alternative — holding on every sole-occupant
// undecryptable slot — would let one genuinely corrupt object wedge its
// device forever, the exact failure this cursor exists to remove.
type seqOutcome struct {
	held     bool
	consumed bool
}

// ApplyEventsWithStats is ApplyEvents plus an ApplyStats report; see
// ApplyEvents for the cursor semantics. after is the transport cursor the
// batch was pulled with (nil means "from the beginning", e.g. single-event
// replays that ignore the returned cursor).
func ApplyEventsWithStats(ctx context.Context, st *state.Store, events []state.Event, after Cursor) (Cursor, ApplyStats, error) {
	var stats ApplyStats
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
	outcomes := map[string]map[int64]*seqOutcome{}
	record := func(event state.Event, consumed bool) {
		if event.Seq <= 0 {
			return // legacy pre-sequence event: never touches the cursor
		}
		slots := outcomes[event.DeviceID]
		if slots == nil {
			slots = map[int64]*seqOutcome{}
			outcomes[event.DeviceID] = slots
		}
		o := slots[event.Seq]
		if o == nil {
			o = &seqOutcome{}
			slots[event.Seq] = o
		}
		if consumed {
			o.consumed = true
		} else {
			o.held = true
		}
	}
	for _, event := range events {
		// P6-SYNC-04: an enc.v2 carrier reaching the apply path is one
		// EncryptedHub.Pull forwarded because AEAD authentication failed on
		// every held key — corruption, forgery, or a hub-side carrier
		// mutation. It is PERMANENTLY unappliable (a re-delivery fails
		// authentication identically), so quarantine it as an undecryptable
		// conflict and advance the cursor past it — never insert it (routing
		// it through insertEvent would backfill a content hash over
		// ciphertext and, pre-enrollment, accept an unknown-device carrier
		// into the log), and never hold the cursor (that would wedge sync on
		// one poisoned object forever).
		if event.Type == EventEncryptedV2 {
			if err := insertUndecryptableEventConflict(ctx, st, event); err != nil {
				return nil, stats, err
			}
			stats.Quarantined++
			// The carrier's Seq slot counts as consumed: the quarantine is a
			// durable, replayable record (ReplayUndecryptableConflicts), so
			// re-delivery would change nothing. Every carrier field, Seq
			// included, is hub-writable (AEAD failed, nothing authenticated);
			// a forged Seq cannot advance past a real event that is in the
			// batch (held dominates consumed at a contested slot — see
			// seqOutcome, including the documented withheld-occupant
			// residual).
			record(event, true)
			continue
		}
		// SYNC-03: quarantine remote events with implausible HLC values
		// (non-positive or below the epoch floor) so they cannot win every
		// same-path conflict from the "past" direction. These are permanently
		// invalid and do NOT hold back the cursor (SYNC-01).
		physical := event.HLC >> hlcLogicalBits
		if event.HLC <= 0 || physical < epochFloorMS {
			if err := quarantineSkewedEvent(ctx, st, event, physical-now); err != nil {
				return nil, stats, err
			}
			stats.Quarantined++
			// Permanently invalid (SYNC-03): consumed — re-delivery would fail
			// identically forever.
			record(event, true)
			continue
		}
		// SYNC-3: quarantine remote events whose physical timestamp is beyond
		// the trusted skew so one bad/malicious peer cannot poison ordering.
		// Skipping (not aborting) keeps the rest of the batch converging. This
		// is TRANSIENT (bounded by maxSkew) so it HOLDS this device's cursor
		// (SYNC-01, per-device since P5-SYNC-01) until local time catches up
		// and the event is re-delivered.
		if offset := physical - now; offset > maxSkewMS {
			if err := quarantineSkewedEvent(ctx, st, event, offset); err != nil {
				return nil, stats, err
			}
			stats.Quarantined++
			record(event, false)
			continue
		}
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
			inserted, err := tx.InsertEvent(ctx, event)
			if err != nil {
				return err
			}
			if !inserted {
				return nil
			}
			if err := tx.ReceiveRemoteHLC(ctx, event.HLC); err != nil {
				return err
			}
			if err := applyEventTx(ctx, tx, event); err != nil {
				return err
			}
			// P6-HUB-01 review: a skew-quarantined delivery of this event left
			// an open untrustworthy_remote_time conflict that nothing else
			// clears; now that the event has actually applied, the quarantine
			// reason is gone — resolve it so it cannot block `hub gc` forever.
			// The details fingerprint is stable (CODE-02) and the resolve is
			// idempotent/no-op when no such conflict exists.
			skewDetails, mErr := json.Marshal(skewConflictDetails{
				EventID:  event.ID,
				DeviceID: event.DeviceID,
				HLC:      event.HLC,
			})
			if mErr != nil {
				return mErr
			}
			if err := tx.ResolveConflictByFingerprint(ctx, ConflictUntrustworthyTime, string(skewDetails),
				`{"action":"auto","reason":"event applied after skew quarantine"}`); err != nil {
				return err
			}
			// P6-SEC-03: an earlier delivery of this event may have broken on
			// the per-device hash chain (its predecessor was quarantined as
			// undecryptable, so the prev-hash had no anchor) and left an open
			// event_hash_chain_break conflict. Now that the event has applied
			// — e.g. after ReplayUndecryptableConflicts recovered the
			// predecessor — that quarantine reason is gone; resolve it so it
			// cannot block `hub gc` forever. Resolution is by event id, not
			// details fingerprint, because hash-chain details embed the
			// volatile cause error. Idempotent/no-op when no such conflict
			// exists.
			return tx.ResolveOpenConflictsByEventID(ctx, ConflictEventHashChain, event.ID,
				`{"action":"auto","reason":"event applied after hash-chain hold (P6-SEC-03)"}`)
		}); err != nil {
			if errors.Is(err, state.ErrEventHashChain) {
				if conflictErr := insertEventHashChainConflict(ctx, st, event, err); conflictErr != nil {
					return nil, stats, errors.Join(err, conflictErr)
				}
				stats.Quarantined++
				// SYNC-05/CODE-01: record the conflict and continue so the
				// rest of the batch (and other devices' events) still apply,
				// mirroring the skew-quarantine path. The broken event is
				// never inserted. SYNC-01: HOLD this device's cursor below it
				// so it is re-delivered next pull (a re-delivery may carry the
				// correct prev_event_hash); insertConflict dedups on stable
				// details so no unbounded growth results.
				record(event, false)
				continue
			}
			if errors.Is(err, state.ErrEventVerification) || errors.Is(err, state.ErrDivergentEvent) {
				if conflictErr := insertEventVerificationConflict(ctx, st, event, err); conflictErr != nil {
					return nil, stats, errors.Join(err, conflictErr)
				}
				stats.Quarantined++
				// Permanent verification/divergence failures are quarantined
				// and counted as consumed for the cursor: re-delivery would
				// fail identically forever (the full event is preserved in the
				// conflict for approval replay).
				record(event, true)
				continue
			}
			return nil, stats, err
		}
		// Applied, or deduped (already inserted): both consume the slot.
		// Deduplication-as-consumption is deliberate (P5-SYNC-01): a device
		// re-pulling events it already holds (its own pushed events, or a
		// pre-migration full re-pull) advances past them instead of
		// re-fetching the same objects on every sync forever.
		record(event, true)
	}
	// P5-SYNC-01: per origin device, advance to the end of the contiguous
	// consumed run starting at after[dev]+1. A held slot (transient skew /
	// hash-chain quarantine) or a Seq gap (an object missing from the hub)
	// stops the run — never advance over either, or the event is permanently
	// stranded (Pull only returns Seq > cursor).
	safe := Cursor{}
	for dev, slots := range outcomes {
		start := after.After(dev)
		last := start
		next := start + 1
		for {
			o, ok := slots[next]
			if !ok || o.held {
				break
			}
			last = next
			next++
		}
		if last > start {
			safe[dev] = last
		}
		var maxSeq int64
		for seq := range slots {
			if seq > maxSeq {
				maxSeq = seq
			}
		}
		if maxSeq > last {
			stats.CursorHeld = true
			reason := "transiently held event"
			if o, ok := slots[next]; !ok || (o != nil && !o.held) {
				reason = "per-device seq gap on hub (missing object?)"
			}
			logging.Logger(ctx).Warn("sync cursor held back for device",
				"device_id", dev, "safe_seq", last, "max_batch_seq", maxSeq, "reason", reason)
		}
	}
	return safe, stats, nil
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
	return st.InsertConflict(ctx, "", ConflictUntrustworthyTime, string(raw))
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
	return st.InsertConflict(ctx, "", ConflictEventHashChain, string(raw))
}

func insertEventVerificationConflict(ctx context.Context, st *state.Store, event state.Event, cause error) error {
	kind := EventConflictKindVerification
	if errors.Is(cause, state.ErrDivergentEvent) {
		kind = EventConflictKindDivergent
	}
	return insertEventConflictOnce(ctx, st, event, kind, cause.Error())
}

// insertUndecryptableEventConflict quarantines an enc.v2 carrier that failed
// AEAD authentication on every held key (P6-SYNC-04). The specific decrypt
// error was logged by EncryptedHub.Pull; the conflict preserves the full
// carrier (EventJSON) for forensics. Never replayed by `devices approve`.
func insertUndecryptableEventConflict(ctx context.Context, st *state.Store, event state.Event) error {
	return insertEventConflictOnce(ctx, st, event, EventConflictKindUndecryptable,
		"enc.v2 AEAD authentication failed on every held key (corruption, forgery, or hub-side carrier mutation)")
}

// insertEventConflictOnce records an event_verification_failure conflict,
// dedupping on (event ID, kind) rather than exact details: the same event
// re-quarantined for the same class of reason (e.g. an approve-time replay
// that fails the signature check where the original failure was pending
// trust — both kind "verification") must not open a second conflict row (the
// error string is volatile, the event is not). The kind IS part of the key so
// the undecryptable replay can apply-then-resolve: a restored carrier that
// fails signature verification records a FRESH "verification" row even while
// its "undecryptable" row is still open (post-#44 review residual, gpt-5.5).
func insertEventConflictOnce(ctx context.Context, st *state.Store, event state.Event, kind, cause string) error {
	existing, err := st.OpenConflictsByType(ctx, ConflictEventVerification)
	if err != nil {
		return err
	}
	for _, c := range existing {
		var d eventVerificationConflictDetails
		if json.Unmarshal([]byte(c.DetailsJSON), &d) == nil && d.EventID == event.ID && d.Kind == kind {
			return nil
		}
	}
	eventRaw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(eventVerificationConflictDetails{
		Kind:      kind,
		EventID:   event.ID,
		DeviceID:  event.DeviceID,
		HLC:       event.HLC,
		Seq:       event.Seq,
		Type:      event.Type,
		Error:     cause,
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
