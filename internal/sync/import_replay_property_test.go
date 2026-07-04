package sync

// import_replay_property_test.go is the rapid property proving the snapshot
// producer/consumer round-trip (BuildSnapshot -> ImportSnapshot) is equivalent
// to event replay: a device bootstrapped from a snapshot, then fed a subset of
// the same events, lands on the SAME active namespace projection as a device
// that replayed the events directly. This is the randomized generalization of
// TestImportThenApplyEqualsApplyThenImport.

import (
	"context"
	"testing"

	"pgregory.net/rapid"
)

// TestImportEqualsReplayRapid is the import≡replay property. Invariant: for any
// convergent event set, ImportSnapshot of the snapshot BuildSnapshot derives
// from a full replay, followed by replay of an arbitrary subset of the same
// events, yields the same active ProjectStatus rows (path, remote_key, source
// HLC/device/event, status) as a plain full replay.
func TestImportEqualsReplayRapid(t *testing.T) {
	// Capped: this property opens 2 migrated sqlite stores per check (see
	// checkBounded); the convergence core is covered at full count by the pure
	// Decide property + fuzz bridge.
	checkBounded(t, 20, func(t *rapid.T) {
		ctx := context.Background()
		events := genEventSet(t)

		// Reference replica A: full replay of a random delivery order.
		stA, _ := newSyncStoreRapid(t)
		orderA := rapid.Permutation(events).Draw(t, "orderA")
		if _, err := ApplyEvents(ctx, stA, cloneEvents(orderA)); err != nil {
			t.Fatalf("replay into A: %v", err)
		}

		// Derive the equivalent snapshot from A's converged state — A IS the
		// converged projection, so its SnapshotEntries/SnapshotTombstones are the
		// canonical winners. Empty floors: no cursor advance is needed for the
		// equivalence check.
		snap, err := BuildSnapshot(ctx, stA, "dev-1", h(1000), Cursor{})
		if err != nil {
			t.Fatalf("BuildSnapshot(A): %v", err)
		}

		// Replica B: bootstrap from the snapshot, then replay a random subset of
		// the same events. Every such event is dominated by the imported terminal
		// coordinates (a stale add/update no-ops on the LWW gate; a different-
		// remote loser no-ops on reconcile; a stale delete no-ops on the live-row
		// gate), so B must stay converged with A.
		stB, _ := newSyncStoreRapid(t)
		if err := ImportSnapshot(ctx, stB, snap, "sha-test", "hub1"); err != nil {
			t.Fatalf("ImportSnapshot(B): %v", err)
		}
		subset := drawSubset(t, events, "subset")
		if _, err := ApplyEvents(ctx, stB, cloneEvents(subset)); err != nil {
			t.Fatalf("replay subset into B: %v", err)
		}

		gotA := activeCanonical(t, stA)
		gotB := activeCanonical(t, stB)
		if gotA != gotB {
			t.Fatalf("import≢replay:\nreplay(A)=%s\nimport+subset(B)=%s\nevents=%s", gotA, gotB, dumpEvents(events))
		}
	})
}
