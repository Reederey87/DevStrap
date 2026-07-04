package sync

// property_helpers_test.go holds the shared machinery for the P4-QUAL-02
// property / model-check layer (rapid-driven): the convergent-event-set
// generator with its ONE documented divergence exclusion, a store harness that
// works from a *rapid.T, a canonical active-projection encoder for cross-store
// equality, and small draw helpers reused by the HLC, Decide-convergence,
// import≡replay, and 3-replica model tests.
//
// GENERATOR EXCLUSIONS (read before widening genEventSet). Both stem from the
// SAME root cause: reconcileSamePath installs the deterministic LOWEST-coordinate
// winner between competing remotes, which is incompatible with same-remote
// last-writer-wins (HIGHEST HLC). Each is pinned by a witness tripwire so the
// exclusion cannot silently outlive the bug.
//
//  1. Delete + different-remote pair on one path (decide.go's documented
//     residual): a delete whose HLC falls between the lowest-coordinate winner
//     and a dropped higher rival flips the terminal state by delivery order.
//     Pinned by TestDecideDifferentRemoteDeleteDivergesWitness.
//
//  2. A single remote carrying MULTIPLE events at different HLCs on a
//     different-remote path — even with NO delete (found by this property layer,
//     reported as a P4-QUAL-02 finding). Same-remote LWW keeps that remote's
//     HIGHEST event, but the cross-remote reconcile keeps the LOWEST coordinate,
//     so whether the reconcile fires before or after the same-remote event
//     reaches its LWW max flips the winner. Pinned by
//     TestDecideDifferentRemoteMultiEventDivergesWitness.
//
// genEventSet therefore restricts its two regimes to the provably-convergent
// subset: (A) single-remote add/update/delete/re-add mixes (PR #87 made these
// converge) and (B) different-remote add/update mixes with EXACTLY ONE event per
// remote and NO deletes (the reconcile then reduces to an order-independent
// global-minimum). Widening either regime past its witness re-introduces a real
// divergence — fix reconcileSamePath first.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"

	"pgregory.net/rapid"

	"github.com/Reederey87/DevStrap/internal/state"
)

// checkBounded runs a rapid property with the number of seeded checks capped at
// maxChecks, but never RAISES a lower `-rapid.checks`/`RAPID_CHECKS` a caller
// set. The DB-backed properties open several migrated sqlite stores per check
// (~0.3s each under `-race`), so at the default 100 they alone dominate the
// package's race time; the convergence logic they exercise is already covered
// exhaustively by the pure Decide property (full default count + the fuzz
// bridge + the 8! anchor), so a smaller round-trip sample suffices. rapid's
// count is a process-global flag, but package tests run sequentially, so the
// save/restore is safe and leaves the cheap pure properties at the full count.
func checkBounded(t *testing.T, maxChecks int, prop func(*rapid.T)) {
	t.Helper()
	f := flag.Lookup("rapid.checks")
	if f == nil {
		rapid.Check(t, prop)
		return
	}
	orig := f.Value.String()
	if cur, err := strconv.Atoi(orig); err != nil || cur > maxChecks {
		_ = f.Value.Set(strconv.Itoa(maxChecks))
		defer func() { _ = f.Value.Set(orig) }()
	}
	rapid.Check(t, prop)
}

// propT is the minimal failure sink satisfied by both *testing.T and *rapid.T,
// so the shared helpers work in plain example tests and rapid properties alike.
type propT interface {
	Fatalf(format string, args ...any)
}

// genEventSet draws a namespace-convergence event set that is guaranteed to
// converge under every delivery order (see the file header for the exclusion).
// It returns events with RAW small HLCs already shifted into the physical band
// (via upsertEvt/deleteEvt) and globally unique event ids.
func genEventSet(t *rapid.T) []state.Event {
	devices := []string{"dev-1", "dev-2", "dev-3"}
	nPaths := rapid.IntRange(1, 4).Draw(t, "n_paths")
	var events []state.Event
	counter := 0
	nextID := func() string { counter++; return fmt.Sprintf("evt-%03d", counter) }
	for p := 0; p < nPaths; p++ {
		path := fmt.Sprintf("work/p%d", p)
		// regime B uses a UNIQUE remote per event (exclusion 2); regime A pins one
		// shared remote so deletes/re-adds stay in the convergent single-remote
		// class (exclusion 1).
		singleRemote := fmt.Sprintf("github.com/x/p%d", p)
		multiRemote := rapid.Bool().Draw(t, fmt.Sprintf("p%d_multiremote", p))
		nEvents := rapid.IntRange(1, 4).Draw(t, fmt.Sprintf("p%d_n", p))
		for e := 0; e < nEvents; e++ {
			// Small HLC range so collisions/ties happen often.
			hlc := int64(rapid.IntRange(1, 6).Draw(t, fmt.Sprintf("p%d_e%d_hlc", p, e)))
			dev := rapid.SampledFrom(devices).Draw(t, fmt.Sprintf("p%d_e%d_dev", p, e))
			id := nextID()
			if multiRemote {
				// Different-remote, delete-free, ONE event per remote: the
				// cross-remote reconcile reduces to an order-independent global
				// minimum. A shared remote here would re-arm exclusion 2.
				typ := rapid.SampledFrom([]string{EventProjectAdded, EventProjectUpdated}).Draw(t, fmt.Sprintf("p%d_e%d_typ", p, e))
				remote := fmt.Sprintf("github.com/x/p%d-e%d", p, e)
				events = append(events, upsertEvt(id, dev, typ, hlc, path, remote))
				continue
			}
			// Single remote: deletes and re-adds are allowed freely (converge).
			kind := rapid.SampledFrom([]string{"add", "update", "delete"}).Draw(t, fmt.Sprintf("p%d_e%d_kind", p, e))
			if kind == "delete" {
				events = append(events, deleteEvt(id, dev, hlc, path))
				continue
			}
			typ := EventProjectAdded
			if kind == "update" {
				typ = EventProjectUpdated
			}
			events = append(events, upsertEvt(id, dev, typ, hlc, path, singleRemote))
		}
	}
	return events
}

// foldDecideErr folds Decide+Apply over a sequence from an empty projection,
// returning the final projection. Unlike foldDecide (decide_property_test.go) it
// takes no *testing.T so it runs inside rapid properties and plain helpers.
func foldDecideErr(events []state.Event) (Projection, error) {
	proj := Projection{}
	for _, ev := range events {
		decision, err := Decide(proj, ev)
		if err != nil {
			return nil, fmt.Errorf("decide %s: %w", ev.ID, err)
		}
		proj, err = proj.Apply(decision)
		if err != nil {
			return nil, fmt.Errorf("apply %s: %w", ev.ID, err)
		}
	}
	return proj, nil
}

// cloneEvents returns a fresh copy of the slice so ApplyEvents' in-place sort
// never reorders a caller's shared backing array between draws.
func cloneEvents(events []state.Event) []state.Event {
	out := make([]state.Event, len(events))
	copy(out, events)
	return out
}

// drawSubset draws an arbitrary subset of events in an arbitrary order, used to
// re-deliver events against an already-converged replica (idempotency).
func drawSubset(t *rapid.T, events []state.Event, label string) []state.Event {
	var chosen []state.Event
	for i, ev := range events {
		if rapid.Bool().Draw(t, fmt.Sprintf("%s_pick_%d", label, i)) {
			chosen = append(chosen, ev)
		}
	}
	return rapid.Permutation(chosen).Draw(t, label+"_order")
}

// splitBatches splits an ordered delivery into sequential ApplyEvents batches
// (1..len contiguous chunks) to model cross-pull-window delivery — exactly where
// the pre-#87 convergence divergence hid.
func splitBatches(t *rapid.T, order []state.Event, label string) [][]state.Event {
	if len(order) == 0 {
		return nil
	}
	var batches [][]state.Event
	cur := []state.Event{order[0]}
	for i := 1; i < len(order); i++ {
		if rapid.Bool().Draw(t, fmt.Sprintf("%s_cut_%d", label, i)) {
			batches = append(batches, cur)
			cur = nil
		}
		cur = append(cur, order[i])
	}
	return append(batches, cur)
}

// newSyncStoreRapid is newSyncStore for rapid properties: a fresh migrated store
// with an unenrolled local device (no approved device exists, so the
// pre-enrollment window accepts the unsigned generated events, matching how the
// example apply tests operate). It uses os.MkdirTemp because *rapid.T has no
// TempDir.
func newSyncStoreRapid(t *rapid.T) (*state.Store, state.Device) {
	dir, err := os.MkdirTemp("", "ds-prop-*")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	st, err := state.Open(context.Background(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := st.EnsureWorkspace(context.Background(), "test", "/tmp/Code"); err != nil {
		t.Fatalf("ensure workspace: %v", err)
	}
	device, err := st.EnsureDevice(context.Background(), "device-a")
	if err != nil {
		t.Fatalf("ensure device: %v", err)
	}
	return st, device
}

// canonicalRow is the convergence-relevant projection of a persisted active
// project row: the fields the audit names (path, remote_key, source HLC/device/
// event, status). Timestamps and materialization state — which legitimately
// differ between the event-apply and snapshot-import write paths — are excluded.
type canonicalRow struct {
	Path                string `json:"path"`
	PathKey             string `json:"path_key"`
	Status              string `json:"status"`
	RemoteKey           string `json:"remote_key"`
	RemoteURL           string `json:"remote_url"`
	Type                string `json:"type"`
	DefaultBranch       string `json:"default_branch"`
	SourceEventHLC      int64  `json:"source_event_hlc"`
	SourceEventDeviceID string `json:"source_event_device_id"`
	SourceEventID       string `json:"source_event_id"`
}

// activeCanonical encodes a store's ACTIVE namespace projection (ListProjects
// returns only active rows) into a deterministic, path-sorted JSON blob so two
// stores can be compared for byte-identical convergence.
func activeCanonical(t propT, st *state.Store) string {
	projects, err := st.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	rows := make([]canonicalRow, 0, len(projects))
	for _, p := range projects {
		rows = append(rows, canonicalRow{
			Path:                p.Path,
			PathKey:             p.PathKey,
			Status:              p.Status,
			RemoteKey:           p.RemoteKey,
			RemoteURL:           p.RemoteURL,
			Type:                p.Type,
			DefaultBranch:       p.DefaultBranch,
			SourceEventHLC:      p.SourceEventHLC,
			SourceEventDeviceID: p.SourceEventDeviceID,
			SourceEventID:       p.SourceEventID,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].PathKey < rows[j].PathKey })
	raw, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatalf("marshal canonical rows: %v", err)
	}
	return string(raw)
}

// dumpEvents renders an event set for failure messages (id, device, type, HLC,
// payload) without the noise of the full struct.
func dumpEvents(events []state.Event) string {
	type e struct {
		ID      string `json:"id"`
		Dev     string `json:"dev"`
		Type    string `json:"type"`
		HLC     int64  `json:"hlc"`
		Payload string `json:"payload"`
	}
	out := make([]e, len(events))
	for i, ev := range events {
		out[i] = e{ID: ev.ID, Dev: ev.DeviceID, Type: ev.Type, HLC: ev.HLC, Payload: ev.PayloadJSON}
	}
	raw, _ := json.MarshalIndent(out, "", "  ")
	return string(raw)
}
