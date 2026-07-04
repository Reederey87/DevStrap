package sync

// decide_rapid_test.go is the randomized (rapid) convergence layer over the
// pure Decide seam, complementing the fixed-batch anchors in
// decide_property_test.go (the 8!-permutation and delete/re-add mix tests, left
// untouched). The generator and its ONE documented exclusion live in
// property_helpers_test.go; the witness test below keeps that exclusion honest.

import (
	"reflect"
	"testing"

	"pgregory.net/rapid"

	"github.com/Reederey87/DevStrap/internal/state"
)

// propDecideConverges is the core strong-eventual-consistency property: folding
// the pure Decide over two independent delivery orders of the SAME event set
// yields the SAME final projection, and re-delivering any event against the
// converged projection is a no-op (idempotency).
func propDecideConverges(t *rapid.T) {
	events := genEventSet(t)
	order1 := rapid.Permutation(events).Draw(t, "order1")
	order2 := rapid.Permutation(events).Draw(t, "order2")

	p1, err := foldDecideErr(order1)
	if err != nil {
		t.Fatalf("fold order1: %v", err)
	}
	p2, err := foldDecideErr(order2)
	if err != nil {
		t.Fatalf("fold order2: %v", err)
	}
	if !reflect.DeepEqual(p1, p2) {
		t.Fatalf("divergence between delivery orders:\np1=%s\np2=%s\nevents=%s",
			dumpProjection(p1), dumpProjection(p2), dumpEvents(events))
	}

	// Idempotency: re-applying any event already reflected in the converged
	// projection leaves it unchanged.
	for _, ev := range events {
		decision, err := Decide(p1, ev)
		if err != nil {
			t.Fatalf("re-decide %s: %v", ev.ID, err)
		}
		next, err := p1.Apply(decision)
		if err != nil {
			t.Fatalf("re-apply %s: %v", ev.ID, err)
		}
		if !reflect.DeepEqual(next, p1) {
			t.Fatalf("re-applying %s changed the converged projection:\ngot=%s\nwant=%s",
				ev.ID, dumpProjection(next), dumpProjection(p1))
		}
	}
}

// TestDecideConvergesRapid runs the convergence property over randomized event
// sets. Invariant: every delivery order of a convergent event set folds to the
// same namespace projection, and duplicates are no-ops.
func TestDecideConvergesRapid(t *testing.T) {
	rapid.Check(t, propDecideConverges)
}

// FuzzDecideConvergence is the coverage-guided bridge for the convergence
// property (`go test -fuzz=FuzzDecideConvergence`): rapid.MakeFuzz turns the
// bitstream the fuzzer mutates into the generator's draws, so libFuzzer-style
// coverage feedback explores the same convergence invariant as TestDecide-
// ConvergesRapid.
func FuzzDecideConvergence(f *testing.F) {
	f.Fuzz(rapid.MakeFuzz(propDecideConverges))
}

// TestDecideDifferentRemoteDeleteDivergesWitness is the TRIPWIRE that keeps the
// generator's one exclusion honest. It pins the exact excluded class — on a
// single path, a delete whose HLC (5) sits BETWEEN a lower-coordinate winner
// (A@2, remote r1) and a dropped higher rival (B@10, remote r2) — and asserts
// that the two delivery orders DIVERGE under the current reconcileSamePath
// (lowest-coordinate winner):
//
//	(A, B, D) -> tombstoned @5   (A wins the reconcile, D@5 then tombstones it)
//	(D, A, B) -> active @10/r2   (D tombstones nothing, A@2 <= tomb is suppressed,
//	                              B@10 > tomb re-adds active)
//
// This divergence is the documented residual the genEventSet exclusion avoids.
// When reconcileSamePath is made HLC-monotonic (winner = HIGHEST coordinate),
// both orders converge on active @10/r2, THIS TEST FAILS, and that failure is
// the signal to delete this test and remove the delete/different-remote
// exclusion from genEventSet.
func TestDecideDifferentRemoteDeleteDivergesWitness(t *testing.T) {
	a := upsertEvt("w-a", "dev-1", EventProjectAdded, 2, "work/witness", "github.com/x/r1")
	b := upsertEvt("w-b", "dev-2", EventProjectAdded, 10, "work/witness", "github.com/x/r2")
	d := deleteEvt("w-d", "dev-3", 5, "work/witness")

	order1, err := foldDecideErr([]state.Event{a, b, d})
	if err != nil {
		t.Fatalf("fold (A,B,D): %v", err)
	}
	order2, err := foldDecideErr([]state.Event{d, a, b})
	if err != nil {
		t.Fatalf("fold (D,A,B): %v", err)
	}

	if tomb, ok := order1.tombstone("work/witness"); !ok || tomb != 5<<hlcLogicalBits {
		t.Fatalf("TRIPWIRE: order (A,B,D) no longer tombstones @5 — reconcileSamePath may now be HLC-monotonic; "+
			"if so, delete this test and drop the delete/different-remote exclusion from genEventSet. got=%+v", order1["work/witness"])
	}
	row, ok := order2.active("work/witness")
	if !ok || row.RemoteKey != "github.com/x/r2" || row.SourceEventHLC != 10<<hlcLogicalBits {
		t.Fatalf("TRIPWIRE: order (D,A,B) no longer active @10/r2 — see above. got=%+v", order2["work/witness"])
	}

	// The two orders MUST diverge: that is the residual this exclusion exists
	// for. If they ever converge, the exclusion is obsolete.
	if reflect.DeepEqual(order1, order2) {
		t.Fatalf("TRIPWIRE FIRED: the delete/different-remote class now CONVERGES; " +
			"remove the exclusion in genEventSet (include deletes with different-remote pairs) and delete this witness test")
	}
}

// TestDecideDifferentRemoteMultiEventDivergesWitness is the TRIPWIRE for the
// SECOND genEventSet exclusion, a divergence this property layer surfaced with
// NO delete involved: on a single path, remote rB carries two adds (@4 and @1)
// while remote rA carries one add (@1). Same-remote LWW keeps rB's HIGHEST
// (@4), but the cross-remote reconcile keeps the LOWEST coordinate, so the
// terminal winner depends on whether the reconcile fires before or after rB
// reaches its LWW max:
//
//	(rB@4, rB@1, rA@1) -> active rA@1   (rB is already @4 when rA@1 reconciles and wins)
//	(rA@1, rB@1, rB@4) -> active rB@4   (rB@1 wins the reconcile tie, then LWW lifts it to @4)
//
// This is the same reconcileSamePath lowest-coordinate root cause as the delete
// residual, but it needs no delete — different-remote convergence is already
// order-dependent whenever a remote has more than one event. When
// reconcileSamePath is made consistent with same-remote LWW, both orders
// converge, THIS TEST FAILS, and that is the signal to delete this test and let
// genEventSet's regime B carry multiple events per remote again.
func TestDecideDifferentRemoteMultiEventDivergesWitness(t *testing.T) {
	// Event ids are chosen so the equal-HLC reconcile tie (rB@1 vs rA@1) resolves
	// toward rB — same HLC and device, so the lowest event id wins, and
	// "wm-1" (rB@1) < "wm-2" (rA@1). That lets rB win the reconcile and then climb
	// to @4 on same-remote LWW in one order but not the other.
	rbHigh := upsertEvt("wm-3", "dev-1", EventProjectAdded, 4, "work/witness2", "github.com/x/rB")
	rbLow := upsertEvt("wm-1", "dev-1", EventProjectAdded, 1, "work/witness2", "github.com/x/rB")
	raLow := upsertEvt("wm-2", "dev-1", EventProjectAdded, 1, "work/witness2", "github.com/x/rA")

	order1, err := foldDecideErr([]state.Event{rbHigh, rbLow, raLow})
	if err != nil {
		t.Fatalf("fold (rB@4,rB@1,rA@1): %v", err)
	}
	order2, err := foldDecideErr([]state.Event{raLow, rbLow, rbHigh})
	if err != nil {
		t.Fatalf("fold (rA@1,rB@1,rB@4): %v", err)
	}

	if row, ok := order1.active("work/witness2"); !ok || row.RemoteKey != "github.com/x/rA" || row.SourceEventHLC != 1<<hlcLogicalBits {
		t.Fatalf("TRIPWIRE: order (rB@4,rB@1,rA@1) no longer active rA@1 — reconcileSamePath may now be LWW-consistent; "+
			"if so, delete this test and let genEventSet regime B carry multiple events per remote. got=%+v", order1["work/witness2"])
	}
	if row, ok := order2.active("work/witness2"); !ok || row.RemoteKey != "github.com/x/rB" || row.SourceEventHLC != 4<<hlcLogicalBits {
		t.Fatalf("TRIPWIRE: order (rA@1,rB@1,rB@4) no longer active rB@4 — see above. got=%+v", order2["work/witness2"])
	}
	if reflect.DeepEqual(order1, order2) {
		t.Fatalf("TRIPWIRE FIRED: the multi-event different-remote class now CONVERGES; " +
			"widen genEventSet regime B back to multiple events per remote and delete this witness test")
	}
}
