package sync

import (
	"encoding/json"
	"fmt"

	"github.com/Reederey87/DevStrap/internal/pathkey"
	"github.com/Reederey87/DevStrap/internal/state"
)

// P5-ARCH-01: the namespace-convergence reconciliation is split into a PURE
// decision (`Decide` over a `Projection` value) and an impure persistence step
// (`applyDecisionTx` in events.go). The decision reads only in-memory values, so
// convergence can be property-tested by folding `Decide` over every permutation
// of a batch (see decide_property_test.go) — the coupling to *state.Tx that let
// the P5-SYNC-02/P5-SYNC-03 convergence bugs ship behind green example tests.
//
// Scope: `Decide` owns the project.added / project.updated / project.deleted
// convergence core — same-remote HLC last-writer-wins, same-path/different-remote
// conflict reconciliation (`reconcileSamePath`), the tombstone HLC gate, and the
// delete-vs-dirty guard. It deliberately does NOT own:
//
//   - project.renamed — its winner/collision decision is fused with an
//     identity-preserving in-place re-key (`tx.RenameProject`) that also carries
//     the linked git_repos / device_project_state / old-path-tombstone rows.
//     A rename event's payload has no remote, so expressing it as pure
//     upsert/tombstone mutations would mint a fresh namespace_entries id and drop
//     the git-repo linkage — a behavior change. Its decision stays in the state
//     layer and its branch stays inline in applyEventTx.
//   - conflict.created / conflict.resolved — conflict-log bookkeeping, not
//     namespace-map convergence (fingerprint match / passthrough).
//   - draft.snapshot.created / device.key.granted — the side-effecting "exotic"
//     branches (blob-ref recording, key-grant audit) that remain inline.
//
// Delete-vs-re-add is HLC-symmetric: a re-add is gated by the standing tombstone
// HLC (decideUpsert) AND a delete is gated by the live row's source-event HLC
// (decideDelete), both as bare-HLC comparisons that resolve exact ties in the
// delete's favor. Every delivery order of a SAME-REMOTE add/update/delete mix
// therefore converges (the P5-ARCH-01 review had surfaced the missing
// delete-side gate as a real strong-eventual-consistency gap; see
// TestDecideConvergesDeleteReaddMix), and live replay now matches
// importTombstoneTx's snapshot-import rule exactly. KNOWN RESIDUAL: when a
// same-path/DIFFERENT-remote pair is in the mix, reconcileSamePath installs the
// deterministic lowest-coordinate winner, so the active row's HLC can sit BELOW
// a dropped rival's — a delete with an HLC between the two still converges to
// different terminal states by delivery order (pre-existing, independent of the
// delete-side gate; tracked as a P4-QUAL-02 follow-up).

// ProjectionRow is the in-memory namespace-entry state `Decide` reads to
// reconcile a single event. It mirrors the subset of namespace_entries (joined
// with git_repos / device_project_state) that governs convergence and holds NO
// database handle. There is at most one row per path_key, whose Status is
// "active" or "deleted"; a deleted row is canonical (only PathKey/Status/
// TombstoneHLC are meaningful — every other field is zero), because a deleted
// row's sole convergence-relevant input is its tombstone HLC.
type ProjectionRow struct {
	NamespaceID         string // stable namespace_entries.id, for conflict attribution
	PathKey             string // case-folded path key
	Path                string // display path
	Type                string
	RemoteURL           string
	RemoteKey           string
	DefaultBranch       string
	Status              string // "active" | "deleted"
	TombstoneHLC        int64  // meaningful only when Status == "deleted"
	SourceEventHLC      int64
	SourceEventDeviceID string
	SourceEventID       string
	DirtyState          string
}

// Projection is the namespace map `Decide` reconciles against, keyed by
// case-folded path_key. In production applyEventTx loads a slice holding just the
// event's path (0 or 1 entries); the property test maintains the full map and
// folds `Decide`+`Apply` over event permutations. `Decide` only ever reads the
// entry for the event's own path, so both usages behave identically.
type Projection map[string]ProjectionRow

// active reports the active row (if any) at a cleaned path key.
func (p Projection) active(key string) (ProjectionRow, bool) {
	row, ok := p[key]
	if !ok || row.Status != projectionStatusActive {
		return ProjectionRow{}, false
	}
	return row, true
}

// tombstone reports the tombstone HLC of a deleted row (if any) at a cleaned key.
func (p Projection) tombstone(key string) (int64, bool) {
	row, ok := p[key]
	if !ok || row.Status != projectionStatusDeleted {
		return 0, false
	}
	return row.TombstoneHLC, true
}

const (
	projectionStatusActive  = "active"
	projectionStatusDeleted = "deleted"
)

// MutationKind enumerates the namespace-map effects Decide can request.
type MutationKind int

const (
	// MutationUpsert activates/updates the row at Upsert.Path with the winner
	// payload and the event's source coordinates.
	MutationUpsert MutationKind = iota
	// MutationTombstone marks the row at Tombstone.Path deleted with a
	// monotonically non-decreasing tombstone HLC.
	MutationTombstone
)

// Mutation is one namespace-map effect expressed as plain data. The impure
// persistence step maps MutationUpsert -> tx.UpsertProject and
// MutationTombstone -> tx.TombstoneProject; the pure Projection.Apply folds them
// in memory.
type Mutation struct {
	Kind      MutationKind
	Upsert    state.UpsertProjectParams // Kind == MutationUpsert
	Tombstone TombstoneMutation         // Kind == MutationTombstone
}

// TombstoneMutation carries a delete/rename-away tombstone.
type TombstoneMutation struct {
	Path string
	HLC  int64
}

// ConflictRecord is a conflict Decide wants recorded, as plain data. The impure
// step maps it to tx.InsertConflict; the pure fold ignores conflicts (they do
// not change the namespace projection).
type ConflictRecord struct {
	NamespaceID string
	Type        string
	Details     string // details_json
}

// Decision is the pure result of reconciling one event against a Projection: the
// intended namespace-map mutations plus the conflicts to record. No database
// access, no I/O, no *state.Tx.
type Decision struct {
	Mutations []Mutation
	Conflicts []ConflictRecord
}

// Decide reconciles one namespace-convergence event (project.added /
// project.updated / project.deleted) against the projection and returns the
// intended effects. It is PURE. Events outside the convergence core return an
// empty Decision (they are dispatched inline in applyEventTx, never here).
func Decide(proj Projection, event state.Event) (Decision, error) {
	switch event.Type {
	case EventProjectAdded, EventProjectUpdated:
		var payload ProjectPayload
		if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err != nil {
			return Decision{}, fmt.Errorf("decode event %s: %w", event.ID, err)
		}
		return decideUpsert(proj, payload, event)
	case EventProjectDeleted:
		var payload ProjectPayload
		if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err != nil {
			return Decision{}, fmt.Errorf("decode event %s: %w", event.ID, err)
		}
		return decideDelete(proj, payload, event)
	default:
		return Decision{}, nil
	}
}

// decideUpsert mirrors the original applyEventTx project.added/updated branch:
// tombstone HLC gate, then same-path/different-remote reconciliation, then
// same-remote HLC last-writer-wins, then plain upsert.
func decideUpsert(proj Projection, payload ProjectPayload, event state.Event) (Decision, error) {
	pk, err := pathkey.Clean(payload.Path)
	if err != nil {
		return Decision{}, err
	}
	// SYNC-03: an active event at or below a standing tombstone is a stale
	// resurrection attempt — no-op so a delete cannot be undone from the past.
	if tombHLC, ok := proj.tombstone(pk.Key); ok && event.HLC <= tombHLC {
		return Decision{}, nil
	}
	existing, hasActive := proj.active(pk.Key)
	if hasActive && existing.RemoteKey != "" && payload.RemoteKey != "" && existing.RemoteKey != payload.RemoteKey {
		winner, incomingWins, details, err := reconcileSamePath(existing.toProjectStatus(), payload, event)
		if err != nil {
			return Decision{}, err
		}
		d := Decision{}
		if incomingWins {
			d.Mutations = append(d.Mutations, Mutation{Kind: MutationUpsert, Upsert: upsertParamsForEvent(winner, event)})
		}
		d.Conflicts = append(d.Conflicts, ConflictRecord{
			NamespaceID: existing.NamespaceID,
			Type:        ConflictSamePathDifferentRemote,
			Details:     details,
		})
		return d, nil
	}
	// SYNC-01: same-remote add/update is HLC last-writer-wins. Only mutate when
	// the incoming coordinates strictly dominate the stored source-event
	// coordinates; otherwise no-op so convergence is arrival-order independent.
	if hasActive {
		cur := samePathCandidate{hlc: existing.SourceEventHLC, deviceID: existing.SourceEventDeviceID, eventID: existing.SourceEventID}
		inc := samePathCandidate{hlc: event.HLC, deviceID: event.DeviceID, eventID: event.ID}
		if !samePathLess(cur, inc) {
			return Decision{}, nil // stored coords dominate -> stale, skip
		}
	}
	return Decision{Mutations: []Mutation{{Kind: MutationUpsert, Upsert: upsertParamsForEvent(payload, event)}}}, nil
}

// decideDelete mirrors importTombstoneTx's precedence exactly: a live add
// strictly newer than the delete keeps the path (LWW — the delete is stale); a
// dirty active checkout raises a pending_delete_conflict and is NOT tombstoned;
// otherwise the path is tombstoned (creating a deleted placeholder when absent).
func decideDelete(proj Projection, payload ProjectPayload, event state.Event) (Decision, error) {
	pk, err := pathkey.Clean(payload.Path)
	if err != nil {
		return Decision{}, err
	}
	if existing, ok := proj.active(pk.Key); ok {
		// Deliberately a bare-HLC comparison, NOT the full (HLC, device, event)
		// coordinate tie-break: the add side resolves add/delete ties by HLC
		// alone in the tombstone's favor (decideUpsert blocks an add when
		// event.HLC <= tombstoneHLC), so an equal-HLC add+delete converges on
		// deleted in both orders only if the delete also wins its tie here.
		// A samePathLess compare would diverge from importTombstoneTx, which
		// pins the same bare-HLC rule for snapshot import.
		if existing.SourceEventHLC > event.HLC {
			return Decision{}, nil // live add strictly newer than the delete → keep it
		}
		if existing.DirtyState == dirtyStateDirty {
			raw, err := json.Marshal(pendingDeleteConflictDetails{
				Path:     payload.Path,
				EventID:  event.ID,
				DeviceID: event.DeviceID,
				HLC:      event.HLC,
			})
			if err != nil {
				return Decision{}, err
			}
			return Decision{Conflicts: []ConflictRecord{{
				NamespaceID: existing.NamespaceID,
				Type:        ConflictPendingDelete,
				Details:     string(raw),
			}}}, nil
		}
	}
	return Decision{Mutations: []Mutation{{Kind: MutationTombstone, Tombstone: TombstoneMutation{Path: payload.Path, HLC: event.HLC}}}}, nil
}

// toProjectStatus adapts a ProjectionRow to the state.ProjectStatus shape the
// already-pure reconcileSamePath consumes (P5-ARCH-01: reuse the exact tested
// winner-selection logic instead of duplicating it).
func (r ProjectionRow) toProjectStatus() state.ProjectStatus {
	return state.ProjectStatus{
		NamespaceEntry: state.NamespaceEntry{
			ID:                  r.NamespaceID,
			Path:                r.Path,
			PathKey:             r.PathKey,
			Type:                r.Type,
			SourceEventHLC:      r.SourceEventHLC,
			SourceEventDeviceID: r.SourceEventDeviceID,
			SourceEventID:       r.SourceEventID,
		},
		RemoteURL:     r.RemoteURL,
		RemoteKey:     r.RemoteKey,
		DefaultBranch: r.DefaultBranch,
		DirtyState:    r.DirtyState,
	}
}

// Apply folds a Decision's mutations into the projection and returns the next
// projection, PURELY (the receiver is not modified). It is the in-memory analogue
// of applyDecisionTx used to property-test convergence; production persists via
// applyDecisionTx instead. Conflicts do not change the namespace projection.
func (p Projection) Apply(d Decision) (Projection, error) {
	next := make(Projection, len(p))
	for k, v := range p {
		next[k] = v
	}
	for _, m := range d.Mutations {
		switch m.Kind {
		case MutationUpsert:
			pk, err := pathkey.Clean(m.Upsert.Path)
			if err != nil {
				return nil, err
			}
			prev := next[pk.Key]
			row := ProjectionRow{
				NamespaceID:         prev.NamespaceID, // preserved: upsert keeps the namespace_entries.id
				PathKey:             pk.Key,
				Path:                pk.Display,
				Type:                m.Upsert.Type,
				RemoteURL:           m.Upsert.RemoteURL,
				RemoteKey:           m.Upsert.RemoteKey,
				DefaultBranch:       m.Upsert.DefaultBranch,
				Status:              projectionStatusActive,
				SourceEventHLC:      m.Upsert.SourceEventHLC,
				SourceEventDeviceID: m.Upsert.SourceEventDeviceID,
				SourceEventID:       m.Upsert.SourceEventID,
			}
			if prev.Status == projectionStatusActive {
				row.DirtyState = prev.DirtyState // upsert does not touch device_project_state
			}
			next[pk.Key] = row
		case MutationTombstone:
			pk, err := pathkey.Clean(m.Tombstone.Path)
			if err != nil {
				return nil, err
			}
			tomb := m.Tombstone.HLC
			if prev, ok := next[pk.Key]; ok && prev.Status == projectionStatusDeleted && prev.TombstoneHLC > tomb {
				tomb = prev.TombstoneHLC // tombstone HLC is monotonically non-decreasing
			}
			next[pk.Key] = ProjectionRow{
				PathKey:      pk.Key,
				Path:         pk.Display,
				Status:       projectionStatusDeleted,
				TombstoneHLC: tomb,
			}
		}
	}
	return next, nil
}
