package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
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
	// ENV-SYNC-01: captured/bound env profile metadata; supersedes the planned "env.profile.bound".
	EventEnvProfileUpdated = "env.profile.updated"
	EventDeviceKeyGranted  = "device.key.granted" // P4-SEC-07: age-wrapped WCK epoch grant
	// TRUST-01: fleet-wide device-trust propagation. Only the fail-safe
	// direction syncs — device.approved is DELIBERATELY not an event, because
	// propagating approvals would let one compromised approved device enroll
	// attacker devices fleet-wide; approval stays the local P4-SEC-04
	// fingerprint ceremony.
	EventDeviceRevoked = "device.revoked"
	EventDeviceLost    = "device.lost"
	// EventGitstateObserved is a signed, read-only git-state snapshot (working-
	// state validation plane Layer A, spec/07): branch, HEAD sha, upstream sha,
	// and dirty/untracked/unmerged/ahead/behind/stash counts. Strictly separate
	// from agent worktree-base resolution (fresh worktrees always base from
	// origin/<default_branch>, never from anything in this plane).
	EventGitstateObserved = "repo.gitstate.observed"
	// EventRepoWipPushed is a signed record of a working-state validation
	// plane Layer B (spec/07) WIP push: `git stash create` produced a commit
	// object without touching the worktree or index, and that object was
	// pushed to refs/devstrap/wip/<device_id>/<path_key> via a raw refspec.
	// Strictly separate from agent worktree-base resolution — refs/devstrap/wip/*
	// must NEVER be read by the fresh-worktree resolver (spec/07's
	// non-negotiable invariant).
	EventRepoWipPushed = "repo.wip.pushed"
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
	// ConflictEventOmission (P4-SYNC-05) is raised when a peer's SIGNED head
	// commits to a seq beyond the contiguous prefix we received (a hub
	// withholding that peer's newest events), or when our independently-folded
	// prefix disagrees with the peer's signed fold at the same seq (a fork /
	// equivocation). Unlike the event-quarantine conflicts, no single event is
	// held — the alarm is that our view is provably incomplete or divergent.
	ConflictEventOmission = "event_omission"
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
	// An open omission alarm means a peer's newest events are provably being
	// withheld (or its stream has forked): the local replica is missing events
	// other devices may reference, so a mark-and-sweep must not run.
	ConflictEventOmission,
}

// defaultReceiveMaxSkew bounds how far ahead of local physical time a remote
// event's HLC may be before it is quarantined instead of applied.
const defaultReceiveMaxSkew = 5 * time.Minute

// devstrapEpochFloorMS is 2024-01-01T00:00:00Z in Unix milliseconds.
const devstrapEpochFloorMS int64 = 1704067200000

// epochFloorMS is the minimum plausible physical HLC timestamp. Components
// below this floor are quarantined as ConflictUntrustworthyTime to stop a peer
// from poisoning ordering from the past direction. These events are
// permanently invalid and consumed, so they do not hold the transport cursor.
// It is a variable so tests using synthetic tiny HLC values can lower it.
var epochFloorMS = devstrapEpochFloorMS

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

// EnvProfilePayload describes a captured or provider-bound env profile.
// Payloads ride the enc.v2 envelope, so var names are not visible to the hub;
// they are required so the apply path can populate secret_bindings without
// decrypting the value blob (ENV-SYNC-01).
type EnvProfilePayload struct {
	Path     string            `json:"path"`
	Profile  string            `json:"profile"`
	Provider string            `json:"provider"`            // devstrap_encrypted or a provider name (e.g. 1password)
	Mode     string            `json:"mode"`                // hydrate_or_runtime | runtime_only
	BlobRef  string            `json:"blob_ref,omitempty"`  // devstrap_encrypted only
	VarNames []string          `json:"var_names,omitempty"` // devstrap_encrypted only
	Refs     map[string]string `json:"refs,omitempty"`      // provider profiles only (var -> op:// ref)
}

// DeviceTrustPayload names the target of a device.revoked/device.lost event
// (TRUST-01). The resulting trust state derives from the event TYPE, not the
// payload, so there is exactly one source of truth. Reason is audit-only.
type DeviceTrustPayload struct {
	DeviceID string `json:"device_id"`
	Reason   string `json:"reason,omitempty"`
}

// GitstatePayload carries a repo.gitstate.observed event (working-state
// validation plane Layer A): a read-only snapshot of one project's git
// working state as observed on the emitting device. The observing device is
// the event's own DeviceID and the observation instant is the event's own
// HLC — neither is duplicated in the payload. Captured with
// `git --no-optional-locks status --porcelain=v2 --branch`, which never
// writes .git/index.
type GitstatePayload struct {
	Path           string `json:"path"`
	Branch         string `json:"branch"`
	HeadSHA        string `json:"head_sha"`
	UpstreamBranch string `json:"upstream_branch,omitempty"`
	UpstreamSHA    string `json:"upstream_sha,omitempty"`
	DirtyCount     int    `json:"dirty_count"`
	UntrackedCount int    `json:"untracked_count"`
	UnmergedCount  int    `json:"unmerged_count"`
	AheadCount     int    `json:"ahead_count"`
	BehindCount    int    `json:"behind_count"`
	StashCount     int    `json:"stash_count"`
}

// WipPayload carries a repo.wip.pushed event (working-state validation plane
// Layer B): the ref and sha a `git stash create` commit was pushed to via a
// raw refspec, plus the base sha it was created from. The observing device is
// the event's own DeviceID and the observation instant is the event's own
// HLC — neither is duplicated in the payload, exactly like GitstatePayload.
type WipPayload struct {
	Path       string `json:"path"`
	Ref        string `json:"ref"`
	SHA        string `json:"sha"`
	BaseSHA    string `json:"base_sha"`
	CapturedAt string `json:"captured_at"`
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
	// EventConflictKindEnvPendingProject quarantines a verified
	// env.profile.updated whose project has not applied yet (ENV-SYNC-01
	// review): recoverable ordering, e.g. the project's origin device is not
	// pinned here yet. Replayed by ReplayPendingProjectConflicts.
	EventConflictKindEnvPendingProject = "env_pending_project"
	// EventConflictKindDraftPendingProject quarantines a verified
	// draft.snapshot.created whose project has not applied yet. This mirrors
	// env_pending_project so one stale/early draft pointer cannot wedge the
	// pull cursor.
	EventConflictKindDraftPendingProject = "draft_pending_project"
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

// NewEnvProfileEvent builds an unsigned env.profile.updated event from a
// pre-marshaled payload (ENV-SYNC-01). The store stamps HLC, seq, device id,
// and the device signature on InsertLocalEvent.
func NewEnvProfileEvent(payloadJSON string) state.Event {
	return state.Event{
		Type:        EventEnvProfileUpdated,
		PayloadJSON: payloadJSON,
		ContentHash: state.ContentHash(payloadJSON),
	}
}

// NewDeviceTrustEvent builds an unsigned device.revoked/device.lost event
// from a pre-marshaled DeviceTrustPayload (TRUST-01). The store stamps HLC,
// seq, device id, and the device signature on InsertLocalEvent.
func NewDeviceTrustEvent(typ, payloadJSON string) state.Event {
	return state.Event{
		Type:        typ,
		PayloadJSON: payloadJSON,
		ContentHash: state.ContentHash(payloadJSON),
	}
}

// NewGitstateEvent builds an unsigned repo.gitstate.observed event from a
// pre-marshaled GitstatePayload (working-state validation plane Layer A). The
// store stamps HLC, seq, device id, and the device signature on
// InsertLocalEvent.
func NewGitstateEvent(payloadJSON string) state.Event {
	return state.Event{
		Type:        EventGitstateObserved,
		PayloadJSON: payloadJSON,
		ContentHash: state.ContentHash(payloadJSON),
	}
}

// NewWipPushedEvent builds an unsigned repo.wip.pushed event from a
// pre-marshaled WipPayload (working-state validation plane Layer B). The
// store stamps HLC, seq, device id, and the device signature on
// InsertLocalEvent.
func NewWipPushedEvent(payloadJSON string) state.Event {
	return state.Event{
		Type:        EventRepoWipPushed,
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
			// P6-SYNC-02: an earlier pull may have dropped this event as
			// skipped (unknown envelope version pre-upgrade, or a garbled
			// object the hub has since replaced); now that it is CONSUMED —
			// applied here, or deduped because it was already inserted — the
			// durable skip record's wedge is over. Clear it in the same
			// transaction (idempotent no-op when no record exists), on both
			// paths: a restored object for an event this device already holds
			// arrives as a dedup, not an insert.
			if err := tx.ClearSkippedEventTx(ctx, event.ID); err != nil {
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
			if errors.Is(err, errEnvProjectPending) || errors.Is(err, errDraftProjectPending) {
				kind := EventConflictKindEnvPendingProject
				if errors.Is(err, errDraftProjectPending) {
					kind = EventConflictKindDraftPendingProject
				}
				if conflictErr := insertPendingProjectConflict(ctx, st, event, kind, err); conflictErr != nil {
					return nil, stats, errors.Join(err, conflictErr)
				}
				stats.Quarantined++
				// Consumed for the cursor like verification quarantines:
				// re-delivery would hit the same missing project, and the full
				// event is preserved in the conflict for the per-cycle replay
				// (ReplayPendingProjectConflicts) once the project applies.
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

// Pending-project sentinels mark verified pointer events whose project row is
// absent WITHOUT a tombstone: the ordering is recoverable — most commonly the
// project.added author is not pinned on this device yet, so its event sits in a
// verification quarantine — and consuming the pointer silently would lose data
// even after the project later replays. The apply loop quarantines these for
// replay instead.
var (
	errEnvProjectPending   = errors.New("env profile project not yet applied")
	errDraftProjectPending = errors.New("draft snapshot project not yet applied")
)

// insertPendingProjectConflict preserves the full pointer event for replay once
// its project applies (ReplayPendingProjectConflicts).
func insertPendingProjectConflict(ctx context.Context, st *state.Store, event state.Event, kind string, cause error) error {
	return insertEventConflictOnce(ctx, st, event, kind, cause.Error())
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
	case EventProjectAdded, EventProjectUpdated, EventProjectDeleted:
		// P5-ARCH-01: the namespace-convergence core (same-remote LWW,
		// same-path/different-remote reconciliation, tombstone HLC gate,
		// delete-vs-dirty guard) is a PURE decision. Load the projection slice
		// for the event's path, decide, then persist the returned effects. The
		// decision (Decide, decide.go) reads only in-memory values, so it can be
		// convergence-property-tested by permutation (decide_property_test.go).
		proj, err := loadNamespaceProjection(ctx, tx, event)
		if err != nil {
			return err
		}
		decision, err := Decide(proj, event)
		if err != nil {
			return err
		}
		return applyDecisionTx(ctx, tx, decision)
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
			return fmt.Errorf("%w: decode draft snapshot event %s: %w", state.ErrEventVerification, event.ID, err)
		}
		// A blob ref that can never pass RecordDraftSnapshotTx's validation is
		// the same malformed-payload class (review finding): validate HERE so
		// it quarantines-as-consumed instead of surfacing as a raw store error
		// that aborts the batch — or, worse, error-loops the pending replay
		// once the project lands.
		if !strings.HasPrefix(payload.BlobRef, "age_blob:") {
			return fmt.Errorf("%w: draft snapshot blob ref %q must use age_blob: prefix", state.ErrEventVerification, payload.BlobRef)
		}
		// DRAFT-02: record the content-addressed blob reference against the
		// project. The blob content is fetched from the hub during sync and
		// extracted during materialization.
		pk, err := pathkey.Clean(payload.Path)
		if err != nil {
			// A verified event whose payload can never apply (unsafe path) is
			// the same malformed-payload class as a decode failure: quarantine
			// as consumed instead of aborting the batch (#133).
			return fmt.Errorf("%w: draft snapshot path %q: %w", state.ErrEventVerification, payload.Path, err)
		}
		project, err := tx.ProjectByPath(ctx, pk.Display)
		if err != nil {
			// Mirror env.profile.updated: a winning delete drops the pointer,
			// while a missing, non-tombstoned project is recoverable ordering
			// and must be quarantined as consumed so the pull cursor advances.
			if _, ok, terr := tx.TombstoneHLC(ctx, pk.Display); terr != nil {
				return terr
			} else if ok {
				return nil
			}
			return fmt.Errorf("%w: %s", errDraftProjectPending, payload.Path)
		}
		return tx.RecordDraftSnapshotTx(ctx, project.ID, payload.BlobRef, payload.ByteSize, payload.FileCount, event)
	case EventEnvProfileUpdated:
		var payload EnvProfilePayload
		if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err != nil {
			// Same malformed-payload convention as the draft case above (#133):
			// only an APPROVED signer can reach here (mustVerify), and a
			// payload that can never decode must quarantine as consumed, not
			// abort the pull batch.
			return fmt.Errorf("%w: decode env profile event %s: %w", state.ErrEventVerification, event.ID, err)
		}
		pk, err := pathkey.Clean(payload.Path)
		if err != nil {
			return fmt.Errorf("%w: env profile path %q: %w", state.ErrEventVerification, payload.Path, err)
		}
		project, err := tx.ProjectByPath(ctx, pk.Display)
		if err != nil {
			// ENV-SYNC-01: distinguish a WINNING DELETE from a project that has
			// not applied yet (Codex review P2). A tombstoned path drops the
			// pointer (the delete won; a re-add + re-capture re-emits). An
			// absent path without a tombstone is recoverable ordering — the
			// batch loop quarantines the event for replay instead of consuming
			// it, and a hard error here would abort the whole pull batch.
			if _, ok, terr := tx.TombstoneHLC(ctx, pk.Display); terr != nil {
				return terr
			} else if ok {
				return nil
			}
			return fmt.Errorf("%w: %s", errEnvProjectPending, payload.Path)
		}
		hlc, deviceID, eventID, ok, err := tx.EnvProfileSourceCoords(ctx, project.ID)
		if err != nil {
			return err
		}
		if ok && !envCoordLess(hlc, deviceID, eventID, event) {
			return nil
		}
		params := state.EnvProfileParams{
			Name:     payload.Profile,
			Provider: payload.Provider,
			Mode:     payload.Mode,
			BlobRef:  payload.BlobRef,
			VarNames: payload.VarNames,
			Refs:     payload.Refs,
		}
		if _, err := tx.UpsertEnvProfileTx(ctx, project.ID, params, event); err != nil {
			return fmt.Errorf("apply env profile for %q: %w", payload.Path, err)
		}
		return nil
	case EventGitstateObserved:
		// Working-state validation plane Layer A: a mirror-only, read-only
		// snapshot. Unlike env.profile.updated/draft.snapshot.created, this does
		// NOT resolve through tx.ProjectByPath — the mirror is keyed on the
		// event's own normalized path, so a peer's gitstate observation applies
		// even when the local project row has not arrived yet (no pending-project
		// quarantine class exists for this event type; see migration 00029).
		var payload GitstatePayload
		if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err != nil {
			return fmt.Errorf("%w: decode gitstate event %s: %w", state.ErrEventVerification, event.ID, err)
		}
		pk, err := pathkey.Clean(payload.Path)
		if err != nil {
			return fmt.Errorf("%w: gitstate path %q: %w", state.ErrEventVerification, payload.Path, err)
		}
		return tx.UpsertDeviceGitstateTx(ctx, event.DeviceID, pk.Key, pk.Display, state.GitstateParams{
			Branch:         payload.Branch,
			HeadSHA:        payload.HeadSHA,
			UpstreamBranch: payload.UpstreamBranch,
			UpstreamSHA:    payload.UpstreamSHA,
			DirtyCount:     payload.DirtyCount,
			UntrackedCount: payload.UntrackedCount,
			UnmergedCount:  payload.UnmergedCount,
			AheadCount:     payload.AheadCount,
			BehindCount:    payload.BehindCount,
			StashCount:     payload.StashCount,
		}, event)
	case EventRepoWipPushed:
		// Working-state validation plane Layer B: a mirror-only, read-only
		// record of a pushed WIP ref. Like EventGitstateObserved, this does NOT
		// resolve through tx.ProjectByPath — the mirror is keyed on the event's
		// own normalized path, so a peer's WIP push applies even when the local
		// project row has not arrived yet (no pending-project quarantine class
		// exists for this event type; see migration 00030).
		var payload WipPayload
		if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err != nil {
			return fmt.Errorf("%w: decode wip event %s: %w", state.ErrEventVerification, event.ID, err)
		}
		pk, err := pathkey.Clean(payload.Path)
		if err != nil {
			return fmt.Errorf("%w: wip path %q: %w", state.ErrEventVerification, payload.Path, err)
		}
		return tx.UpsertDeviceWipTx(ctx, event.DeviceID, pk.Key, pk.Display, state.WipParams{
			Ref:        payload.Ref,
			SHA:        payload.SHA,
			BaseSHA:    payload.BaseSHA,
			CapturedAt: payload.CapturedAt,
		}, event)
	case EventDeviceRevoked, EventDeviceLost:
		// TRUST-01: a synced trust flip. Signature verification already ran
		// (mustVerifyEvent), so the SIGNER is a locally-approved device; the
		// TARGET may be unknown here (device records do not sync), so ensure a
		// placeholder row exists for the sticky update to act on —
		// pending -> revoked is the fail-closed direction. The update itself
		// is monotonic (only pending/approved flip; the local device never
		// flips from a remote event) and replay-idempotent; needs_rotation is
		// flagged only when a row ACTUALLY changed, so replays can never
		// re-flag values an operator already rotated and cleared.
		var payload DeviceTrustPayload
		// A malformed payload here is permanently malformed and only an
		// APPROVED signer can produce it (mustVerify) — exactly the
		// compromised-device class trust events exist to cut off. Wrap in
		// ErrEventVerification so it quarantines-as-consumed (preserved in a
		// conflict) instead of aborting the batch and wedging the pull
		// (reviewer finding; the env/draft malformed-payload convention is
		// tracked in #133).
		if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err != nil {
			return fmt.Errorf("%w: decode trust event %s: %w", state.ErrEventVerification, event.ID, err)
		}
		if payload.DeviceID == "" {
			return fmt.Errorf("%w: trust event %s: empty target device id", state.ErrEventVerification, event.ID)
		}
		if err := tx.EnsureRemoteDeviceTx(ctx, payload.DeviceID); err != nil {
			return err
		}
		trustState := "revoked"
		if event.Type == EventDeviceLost {
			trustState = "lost"
		}
		// P7-SYNC-02: carry the revocation event's signed HLC so the target's
		// revocation boundary is recorded; the apply path admits that device's
		// pre-boundary events regardless of delivery order.
		changed, err := tx.ApplyRemoteDeviceTrustTx(ctx, payload.DeviceID, trustState, event.HLC)
		if err != nil {
			return err
		}
		if changed {
			if _, err := tx.MarkEncryptedBindingsNeedingRotationTx(ctx); err != nil {
				return err
			}
			// P7-SYNC-04: a device that only LEARNS of a revocation (rather than
			// running it) still owes the forward-secrecy rotation — otherwise, if
			// the revoker's own rotation failed and it went offline, the fleet
			// keeps sealing under the epoch the revoked device holds. Arm the
			// owed-rotation marker transactionally with the flip; the receiver's
			// next sync rotation gate mints epoch+1 excluding the revoked device.
			// Guarded on epoch>0 inside the helper (a keyless device holds no key
			// to protect and its rotation gate skips epoch 0).
			epoch, eerr := tx.CurrentKeyEpochTx(ctx)
			if eerr != nil {
				return eerr
			}
			if err := tx.SetWCKRotationPendingTx(ctx, epoch); err != nil {
				return err
			}
		}
		return nil
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

// envCoordLess uses the same highest-(HLC, deviceID, eventID)-wins ordering as
// samePathLess so env profile LWW converges with namespace reconciliation.
func envCoordLess(hlc int64, deviceID, eventID string, event state.Event) bool {
	return samePathLess(
		samePathCandidate{hlc: hlc, deviceID: deviceID, eventID: eventID},
		samePathCandidate{hlc: event.HLC, deviceID: event.DeviceID, eventID: event.ID},
	)
}

// ReplayPendingProjectConflicts re-attempts every open env_pending_project and
// draft_pending_project quarantine: a verified env.profile.updated or
// draft.snapshot.created whose project had not applied yet was preserved here
// instead of being consumed. Once the project lands — a later pull window, or a
// quarantined project.added replayed by `devices approve` — the stored event
// applies through the normal verified path and the conflict resolves. A
// still-missing project re-quarantines as a dedup no-op and the row stays open
// (visible, bounded). The conflict resolves only AFTER a successful apply,
// mirroring ReplayUndecryptableConflicts' resolve-after-apply rule.
func ReplayPendingProjectConflicts(ctx context.Context, st *state.Store) (int, error) {
	conflicts, err := st.OpenConflictsByType(ctx, ConflictEventVerification)
	if err != nil {
		return 0, err
	}
	replayed := 0
	for _, c := range conflicts {
		var details eventVerificationConflictDetails
		if json.Unmarshal([]byte(c.DetailsJSON), &details) != nil || !isPendingProjectConflictKind(details.Kind) {
			continue
		}
		var restored state.Event
		if err := json.Unmarshal([]byte(details.EventJSON), &restored); err != nil {
			logging.Logger(ctx).Warn("pending-project replay: conflict carries unparseable event JSON",
				"conflict_id", c.ID, "kind", details.Kind, "event_id", details.EventID, "err", err.Error())
			continue
		}
		_, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{restored}, nil)
		if err != nil {
			return replayed, err // conflict stays open; retried next cycle
		}
		if stats.Quarantined > 0 {
			continue // project still missing (or a fresh verification failure); row stays open
		}
		if err := st.ResolveConflict(ctx, c.ID, `{"action":"auto","reason":"project applied after pending-project hold replay"}`); err != nil {
			return replayed, err
		}
		logging.Logger(ctx).Info("pending-project replay: quarantined pointer recovered",
			"kind", details.Kind, "event_id", details.EventID, "device_id", details.DeviceID)
		replayed++
	}
	return replayed, nil
}

func isPendingProjectConflictKind(kind string) bool {
	return kind == EventConflictKindEnvPendingProject || kind == EventConflictKindDraftPendingProject
}

// loadNamespaceProjection loads the projection slice Decide needs for a
// namespace-convergence event (project.added/updated/deleted): the single row
// (active or tombstoned) at the event's path, keyed by path_key. An absent path
// yields an empty projection. This is the impure read half of the P5-ARCH-01
// seam — everything downstream of it (Decide) is pure.
func loadNamespaceProjection(ctx context.Context, tx *state.Tx, event state.Event) (Projection, error) {
	var payload ProjectPayload
	if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err != nil {
		return nil, fmt.Errorf("decode event %s: %w", event.ID, err)
	}
	pk, err := pathkey.Clean(payload.Path)
	if err != nil {
		return nil, err
	}
	proj := Projection{}
	// An active row (if any) supplies the remote/source-event coordinates for
	// reconciliation and the dirty flag for the delete guard.
	if active, err := tx.ProjectByPath(ctx, payload.Path); err == nil {
		proj[pk.Key] = projectionRowFromStatus(active)
		return proj, nil
	}
	// No active row: a standing tombstone (if any) supplies the HLC gate. An
	// active row never coexists with a tombstone (upsert clears tombstone_hlc),
	// so this read is only reached when nothing active was found.
	tombHLC, ok, err := tx.TombstoneHLC(ctx, payload.Path)
	if err != nil {
		return nil, err
	}
	if ok {
		proj[pk.Key] = ProjectionRow{PathKey: pk.Key, Path: pk.Display, Status: projectionStatusDeleted, TombstoneHLC: tombHLC}
	}
	return proj, nil
}

// projectionRowFromStatus adapts a persisted active ProjectStatus into the
// in-memory ProjectionRow Decide reconciles against.
func projectionRowFromStatus(p state.ProjectStatus) ProjectionRow {
	return ProjectionRow{
		NamespaceID:         p.ID,
		PathKey:             p.PathKey,
		Path:                p.Path,
		Type:                p.Type,
		RemoteURL:           p.RemoteURL,
		RemoteKey:           p.RemoteKey,
		DefaultBranch:       p.DefaultBranch,
		Status:              projectionStatusActive,
		SourceEventHLC:      p.SourceEventHLC,
		SourceEventDeviceID: p.SourceEventDeviceID,
		SourceEventID:       p.SourceEventID,
		DirtyState:          p.DirtyState,
	}
}

// applyDecisionTx persists a pure Decision (the impure write half of the
// P5-ARCH-01 seam): mutations first (upsert/tombstone), then conflicts, matching
// the original inline ordering exactly (winner upsert precedes the
// same-path/different-remote conflict insert).
func applyDecisionTx(ctx context.Context, tx *state.Tx, decision Decision) error {
	for _, m := range decision.Mutations {
		switch m.Kind {
		case MutationUpsert:
			if _, err := tx.UpsertProject(ctx, m.Upsert); err != nil {
				return err
			}
		case MutationTombstone:
			if err := tx.TombstoneProject(ctx, m.Tombstone.Path, m.Tombstone.HLC); err != nil {
				return err
			}
		}
	}
	for _, c := range decision.Conflicts {
		if err := tx.InsertConflict(ctx, c.NamespaceID, c.Type, c.Details); err != nil {
			return err
		}
	}
	return nil
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

// reconcileSamePath picks the canonical winner between two remotes claiming
// the same path: the HIGHEST (HLC, deviceID, eventID) coordinate. Highest is
// load-bearing — it keeps the installed row's source HLC monotone with the
// events applied, consistent with same-remote last-writer-wins (decideUpsert)
// and snapshot import (importEntryTx), so different-remote mixes converge in
// every delivery order.
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
	if samePathLess(current, next) {
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
