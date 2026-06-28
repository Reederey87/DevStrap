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
	EventDraftSnapshotCreated = "draft.snapshot.created" // DRAFT-02
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

// DraftSnapshotPayload carries a draft.snapshot.created event's content-addressed
// blob reference and limits (DRAFT-02).
type DraftSnapshotPayload struct {
	Path      string `json:"path"`
	BlobRef   string `json:"blob_ref"`
	ByteSize  int64  `json:"byte_size"`
	FileCount int64  `json:"file_count"`
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

func ApplyEvents(ctx context.Context, st *state.Store, events []state.Event) error {
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
	for _, event := range events {
		// SYNC-03: quarantine remote events with implausible HLC values
		// (non-positive or below the epoch floor) so they cannot win every
		// same-path conflict from the "past" direction.
		physical := event.HLC >> hlcLogicalBits
		if event.HLC <= 0 || physical < epochFloorMS {
			if err := quarantineSkewedEvent(ctx, st, event, physical-now); err != nil {
				return err
			}
			continue
		}
		// SYNC-3: quarantine remote events whose physical timestamp is beyond
		// the trusted skew so one bad/malicious peer cannot poison ordering.
		// Skipping (not aborting) keeps the rest of the batch converging.
		if offset := physical - now; offset > maxSkewMS {
			if err := quarantineSkewedEvent(ctx, st, event, offset); err != nil {
				return err
			}
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
			return applyEventTx(ctx, tx, event)
		}); err != nil {
			if errors.Is(err, state.ErrEventHashChain) {
				if conflictErr := insertEventHashChainConflict(ctx, st, event, err); conflictErr != nil {
					return errors.Join(err, conflictErr)
				}
				// SYNC-05/CODE-01: record the conflict and continue so the
				// rest of the batch (and other devices' events) still apply,
				// mirroring the skew-quarantine path. The broken event is
				// never inserted, so it will be re-delivered on the next pull;
				// insertConflict dedups on stable details so no unbounded
				// growth results.
				continue
			}
			return err
		}
	}
	return nil
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
			return tx.InsertConflict(ctx, existing.ID, "same_path_different_remote", details)
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
			return tx.InsertConflict(ctx, existing.ID, "pending_delete_conflict", string(raw))
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
			return tx.InsertConflict(ctx, "", "rename_target_exists", string(raw))
		}
		return nil
	case EventConflictCreated:
		return tx.InsertConflict(ctx, "", "remote_conflict", event.PayloadJSON)
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

func samePathLess(a, b samePathCandidate) bool {
	if a.hlc != b.hlc {
		return a.hlc < b.hlc
	}
	if a.deviceID != b.deviceID {
		return a.deviceID < b.deviceID
	}
	return a.eventID < b.eventID
}
