package sync

// replica_model_test.go is the P4-QUAL-02 core ask: a small-scope 3-replica
// model check (the hypothesis is that 3 replicas suffice to expose ordering
// divergence). One shared event set is delivered to three independent replicas,
// each in its own random order SPLIT into sequential ApplyEvents batches —
// modelling cross-pull-window delivery, which is exactly where the pre-#87
// convergence divergence hid behind whole-batch example tests.

import (
	"context"
	"testing"

	"pgregory.net/rapid"
)

// TestThreeReplicaConvergenceRapid is the strong-eventual-consistency model
// check. Invariant: three replicas that each receive the same event set in an
// independent order, split across independent batch boundaries, converge to
// byte-identical active ProjectStatus rows — even when one replica re-delivers a
// duplicate subset (idempotency) and another runs tombstone GC (which purges
// deleted rows only, never the active projection).
func TestThreeReplicaConvergenceRapid(t *testing.T) {
	// Capped: this property opens 3 migrated sqlite stores per check (see
	// checkBounded); the convergence core is covered at full count by the pure
	// Decide property + fuzz bridge.
	checkBounded(t, 15, func(t *rapid.T) {
		ctx := context.Background()
		events := genEventSet(t)

		canon := make([]string, 3)
		for r := 0; r < 3; r++ {
			label := replicaLabel(r)
			st, _ := newSyncStoreRapid(t)

			order := rapid.Permutation(events).Draw(t, label+"_order")
			for _, batch := range splitBatches(t, order, label) {
				if _, err := ApplyEvents(ctx, st, cloneEvents(batch)); err != nil {
					t.Fatalf("%s apply batch: %v", label, err)
				}
			}

			switch r {
			case 0:
				// Idempotency: re-deliver an arbitrary subset against the already
				// converged replica; it must not perturb the projection.
				dup := drawSubset(t, events, label+"_dup")
				if _, err := ApplyEvents(ctx, st, cloneEvents(dup)); err != nil {
					t.Fatalf("%s duplicate re-delivery: %v", label, err)
				}
			case 2:
				// Tombstone GC interleaving. GCTombstones deletes only rows with
				// status='deleted' below the given HLC — it can never touch an
				// active row — so we assert equality of the ACTIVE projection
				// only. h(1000) is far above every generated tombstone HLC (<=
				// 6), so this purges the full tombstone set on this replica while
				// the active rows must stay identical to the other replicas'.
				if _, err := st.GCTombstones(ctx, h(1000)); err != nil {
					t.Fatalf("%s GCTombstones: %v", label, err)
				}
			}

			canon[r] = activeCanonical(t, st)
		}

		if canon[0] != canon[1] || canon[1] != canon[2] {
			t.Fatalf("replicas diverged:\nr0=%s\nr1=%s\nr2=%s\nevents=%s",
				canon[0], canon[1], canon[2], dumpEvents(events))
		}
	})
}

func replicaLabel(r int) string {
	return [...]string{"r0", "r1", "r2"}[r]
}
