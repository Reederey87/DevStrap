package sync

// decide_rapid_test.go is the randomized (rapid) convergence layer over the
// pure Decide seam, complementing the fixed-batch anchors in
// decide_property_test.go (the 8!-permutation and delete/re-add mix tests, left
// untouched). The generator lives in property_helpers_test.go and draws from
// the full event space — deletes and multi-event remotes mixed freely across
// different-remote paths — since the reconcileSamePath HLC-monotonic winner
// made every delivery order converge (the former exclusions and their witness
// tripwires are retired).

import (
	"reflect"
	"testing"

	"pgregory.net/rapid"
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
