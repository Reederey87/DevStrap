package sync

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
)

// P5-ARCH-01: the namespace-convergence core is now a PURE Decide over a
// Projection value, so convergence can be property-tested by folding Decide over
// every permutation of a batch — the structural coupling to *state.Tx is what let
// the P5-SYNC-02/P5-SYNC-03 convergence bugs ship behind green example tests.
//
// The fixed event set below is constructed so that each path converges under the
// CURRENT (unchanged) reconciliation semantics. Because Decide only ever reads
// the entry for the event's own path, distinct paths reconcile independently, so
// the whole-projection fold converges iff every per-path subset does. The set
// exercises:
//
//   - same-remote HLC last-writer-wins (highest coordinates win): work/lww;
//   - same-path / different-remote conflict reconciliation (deterministic
//     lowest-coordinate winner, per reconcileSamePath): work/conf;
//   - delete tombstoning with a delete HLC above every add on the path so the
//     tombstone dominates: work/del;
//   - an isolated single add: work/solo.
//
// It deliberately omits a delete mixed with a strictly-higher re-add on ONE path:
// a delete tombstones unconditionally while a re-add is gated by the tombstone
// HLC, so that specific interaction is order-sensitive by construction and sits
// outside the pure convergence core Decide owns (see decide.go scope note).

func upsertEvt(id, dev, typ string, hlc int64, path, remoteKey string) state.Event {
	payload := ProjectPayload{Path: path, Type: "git_repo"}
	if remoteKey != "" {
		payload.RemoteKey = remoteKey
		payload.RemoteURL = "https://example.com/" + remoteKey
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return state.Event{
		ID:          id,
		DeviceID:    dev,
		HLC:         hlc << hlcLogicalBits,
		Type:        typ,
		PayloadJSON: string(raw),
		ContentHash: state.ContentHash(string(raw)),
	}
}

func deleteEvt(id, dev string, hlc int64, path string) state.Event {
	payload := ProjectPayload{Path: path}
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return state.Event{
		ID:          id,
		DeviceID:    dev,
		HLC:         hlc << hlcLogicalBits,
		Type:        EventProjectDeleted,
		PayloadJSON: string(raw),
		ContentHash: state.ContentHash(string(raw)),
	}
}

// convergentEventSet is the fixed batch every permutation is folded over.
func convergentEventSet() []state.Event {
	return []state.Event{
		// work/lww — same remote, HLC last-writer-wins → the @3 event wins.
		upsertEvt("evt-lww-1", "dev-1", EventProjectAdded, 1, "work/lww", "github.com/x/lww"),
		upsertEvt("evt-lww-2", "dev-1", EventProjectUpdated, 3, "work/lww", "github.com/x/lww"),
		upsertEvt("evt-lww-3", "dev-2", EventProjectUpdated, 2, "work/lww", "github.com/x/lww"),
		// work/conf — different remotes → reconcileSamePath's deterministic
		// lowest-coordinate winner (@2, remote conf-a).
		upsertEvt("evt-conf-1", "dev-1", EventProjectAdded, 2, "work/conf", "github.com/x/conf-a"),
		upsertEvt("evt-conf-2", "dev-2", EventProjectAdded, 4, "work/conf", "github.com/x/conf-b"),
		// work/del — delete HLC (5) dominates the add (2) → tombstoned.
		upsertEvt("evt-del-1", "dev-1", EventProjectAdded, 2, "work/del", "github.com/x/del"),
		deleteEvt("evt-del-2", "dev-2", 5, "work/del"),
		// work/solo — isolated single add.
		upsertEvt("evt-solo-1", "dev-1", EventProjectAdded, 3, "work/solo", "github.com/x/solo"),
	}
}

// foldDecide folds Decide+Apply over a sequence of events from an empty
// projection, returning the final projection.
func foldDecide(t *testing.T, events []state.Event) Projection {
	t.Helper()
	proj := Projection{}
	for _, ev := range events {
		decision, err := Decide(proj, ev)
		if err != nil {
			t.Fatalf("Decide(%s): %v", ev.ID, err)
		}
		proj, err = proj.Apply(decision)
		if err != nil {
			t.Fatalf("Apply(%s): %v", ev.ID, err)
		}
	}
	return proj
}

// permute visits every permutation of events (Heap's algorithm) for small N, or
// a deterministic seeded-shuffle sample for larger N, passing a fresh copy each
// time. It is a self-contained, stdlib-only permutation generator.
func permute(events []state.Event, visit func([]state.Event)) {
	n := len(events)
	if n > 8 {
		// Seeded-shuffle sampling keeps the property test bounded for larger
		// batches (deterministic: a fixed LCG, no math/rand global state).
		a := make([]state.Event, n)
		copy(a, events)
		seed := uint64(0x9e3779b97f4a7c15)
		for s := 0; s < 20000; s++ {
			for i := n - 1; i > 0; i-- {
				seed = seed*6364136223846793005 + 1442695040888963407
				j := int(seed>>33) % (i + 1)
				a[i], a[j] = a[j], a[i]
			}
			cp := make([]state.Event, n)
			copy(cp, a)
			visit(cp)
		}
		return
	}
	a := make([]state.Event, n)
	copy(a, events)
	c := make([]int, n)
	cp := make([]state.Event, n)
	copy(cp, a)
	visit(cp)
	for i := 0; i < n; {
		if c[i] < i {
			if i%2 == 0 {
				a[0], a[i] = a[i], a[0]
			} else {
				a[c[i]], a[i] = a[i], a[c[i]]
			}
			out := make([]state.Event, n)
			copy(out, a)
			visit(out)
			c[i]++
			i = 0
		} else {
			c[i] = 0
			i++
		}
	}
}

// TestDecideConvergesUnderEveryPermutation is the P5-ARCH-01 convergence
// property: folding the pure Decide over ANY delivery order of the same batch
// yields the SAME final namespace projection.
func TestDecideConvergesUnderEveryPermutation(t *testing.T) {
	events := convergentEventSet()

	// The canonical result: fold in declaration order.
	want := foldDecide(t, events)

	// Sanity-check the winners so a direction regression (e.g. LWW picking the
	// oldest, or reconcile picking the wrong remote) fails loudly rather than
	// silently "converging" to the wrong state.
	if row, ok := want.active("work/lww"); !ok || row.SourceEventID != "evt-lww-2" || row.RemoteKey != "github.com/x/lww" {
		t.Fatalf("work/lww winner = %+v, want the @3 event (evt-lww-2)", want["work/lww"])
	}
	if row, ok := want.active("work/conf"); !ok || row.RemoteKey != "github.com/x/conf-a" || row.SourceEventID != "evt-conf-1" {
		t.Fatalf("work/conf winner = %+v, want the @2 event (evt-conf-1, conf-a)", want["work/conf"])
	}
	if tomb, ok := want.tombstone("work/del"); !ok || tomb != 5<<hlcLogicalBits {
		t.Fatalf("work/del = %+v, want tombstoned @5", want["work/del"])
	}
	if _, ok := want.active("work/del"); ok {
		t.Fatalf("work/del must be tombstoned, not active: %+v", want["work/del"])
	}
	if row, ok := want.active("work/solo"); !ok || row.RemoteKey != "github.com/x/solo" {
		t.Fatalf("work/solo = %+v, want active with remote solo", want["work/solo"])
	}

	var count int
	permute(events, func(perm []state.Event) {
		count++
		got := foldDecide(t, perm)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("permutation %d diverged:\n got=%s\nwant=%s\norder=%s", count, dumpProjection(got), dumpProjection(want), orderOf(perm))
		}
	})
	if count != 40320 { // 8! — every ordering of the 8-event batch was folded.
		t.Fatalf("visited %d permutations, want 40320 (8!)", count)
	}
}

// TestDecideIsIdempotentOnDuplicate is the P5-ARCH-01 idempotency property:
// re-delivering ANY event that is already reflected in the converged projection
// leaves the projection unchanged.
func TestDecideIsIdempotentOnDuplicate(t *testing.T) {
	events := convergentEventSet()
	converged := foldDecide(t, events)

	for _, ev := range events {
		decision, err := Decide(converged, ev)
		if err != nil {
			t.Fatalf("Decide(%s): %v", ev.ID, err)
		}
		next, err := converged.Apply(decision)
		if err != nil {
			t.Fatalf("Apply(%s): %v", ev.ID, err)
		}
		if !reflect.DeepEqual(next, converged) {
			t.Fatalf("re-applying %s changed the converged projection:\n got=%s\nwant=%s",
				ev.ID, dumpProjection(next), dumpProjection(converged))
		}
	}
}

// dumpProjection renders a projection deterministically for failure messages.
func dumpProjection(p Projection) string {
	raw, _ := json.MarshalIndent(p, "", "  ")
	return string(raw)
}

// orderOf renders a permutation's event ids for failure messages.
func orderOf(events []state.Event) string {
	ids := make([]string, len(events))
	for i, e := range events {
		ids[i] = e.ID
	}
	raw, _ := json.Marshal(ids)
	return string(raw)
}
