package state

import (
	"context"
	"path/filepath"
	"testing"
)

// P5-SYNC-01: hub_device_cursors is the per-origin-device transport cursor.
// Advances must be forward-only per (hub, device); the push watermark is a
// "push:<hubID>" row keyed by the gapless local Seq, backfilled once from the
// legacy HLC watermark; and LocalPendingEventsBySeq must survive a local HLC
// regression that the retired `hlc >` selection would have stranded events
// behind.

func openCursorTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(context.Background(), "personal", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestHubDeviceCursorForwardOnly(t *testing.T) {
	ctx := context.Background()
	st := openCursorTestStore(t)
	const hubID = "file:/tmp/hub.json"

	if err := st.AdvanceHubDeviceCursor(ctx, hubID, "dev_a", 5); err != nil {
		t.Fatal(err)
	}
	if err := st.AdvanceHubDeviceCursor(ctx, hubID, "dev_b", 2); err != nil {
		t.Fatal(err)
	}
	// A stale re-pull must never regress the cursor.
	if err := st.AdvanceHubDeviceCursor(ctx, hubID, "dev_a", 3); err != nil {
		t.Fatal(err)
	}
	cursors, err := st.HubDeviceCursors(ctx, hubID)
	if err != nil {
		t.Fatal(err)
	}
	if cursors["dev_a"] != 5 || cursors["dev_b"] != 2 {
		t.Fatalf("cursors = %v, want dev_a:5 dev_b:2 (forward-only)", cursors)
	}
	// Push rows live under a different hub_id and must not leak into the pull
	// cursor map.
	if err := st.AdvanceHubDeviceCursor(ctx, "push:"+hubID, "dev_local", 9); err != nil {
		t.Fatal(err)
	}
	cursors, err = st.HubDeviceCursors(ctx, hubID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cursors["dev_local"]; ok || len(cursors) != 2 {
		t.Fatalf("cursors = %v, want push watermark rows excluded", cursors)
	}
}

func TestHasHubDeviceCursors(t *testing.T) {
	ctx := context.Background()
	st := openCursorTestStore(t)
	const hubID = "file:/tmp/hub.json"

	has, err := st.HasHubDeviceCursors(ctx, hubID)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Fatal("HasHubDeviceCursors on a fresh store = true, want false")
	}
	// A push watermark alone counts: the device has observed hub interaction.
	if err := st.AdvanceHubDeviceCursor(ctx, "push:"+hubID, "dev_local", 1); err != nil {
		t.Fatal(err)
	}
	has, err = st.HasHubDeviceCursors(ctx, hubID)
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Fatal("HasHubDeviceCursors after a push advance = false, want true (founder gate)")
	}
	// Another hub's rows must not count.
	has, err = st.HasHubDeviceCursors(ctx, "file:/tmp/other.json")
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Fatal("HasHubDeviceCursors leaked across hub IDs")
	}
}

// seedLocalEvents inserts hash-chained events for the local device with
// explicit (seq, hlc) pairs, bypassing stamping so an HLC regression relative
// to seq order can be constructed.
func seedLocalEvents(t *testing.T, st *Store, pairs [][2]int64) Device {
	t.Helper()
	ctx := context.Background()
	device, err := st.EnsureDevice(ctx, "test-host")
	if err != nil {
		t.Fatal(err)
	}
	prevHash := ""
	if err := st.WithTx(ctx, func(tx *Tx) error {
		for i, p := range pairs {
			payload := `{"path":"work/p` + string(rune('a'+i)) + `"}`
			ev := Event{
				ID:            "evt_seed_" + string(rune('a'+i)),
				DeviceID:      device.ID,
				Seq:           p[0],
				HLC:           p[1],
				Type:          "project.added",
				PayloadJSON:   payload,
				ContentHash:   ContentHash(payload),
				PrevEventHash: prevHash,
			}
			inserted, err := tx.InsertEvent(ctx, ev)
			if err != nil {
				return err
			}
			if !inserted {
				t.Fatalf("seed event %d not inserted", i)
			}
			prevHash = ev.ContentHash
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return device
}

func TestLocalPendingEventsBySeqSurvivesHLCRegression(t *testing.T) {
	ctx := context.Background()
	st := openCursorTestStore(t)
	// Seq 1 at HLC 100, then seq 2 at the REGRESSED HLC 50: the retired
	// `hlc > watermark` selection (watermark 100 after pushing seq 1) would
	// silently strand seq 2 forever.
	seedLocalEvents(t, st, [][2]int64{{1, 100}, {2, 50}})

	pending, err := st.LocalPendingEventsBySeq(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Seq != 2 {
		t.Fatalf("pending after seq 1 = %+v, want exactly the regressed-HLC seq 2 event", pending)
	}
}

func TestPushSeqCursorIgnoresLegacyHLCWatermark(t *testing.T) {
	ctx := context.Background()
	st := openCursorTestStore(t)
	const hubID = "file:/tmp/hub.json"
	// Seq 2 carries a REGRESSED HLC below the legacy push watermark: under the
	// retired `hlc >` selection it was silently stranded, and a backfill that
	// inferred "pushed" from `hlc <= watermark` would bake that loss in
	// forever (post-#59 Codex review, P2). The watermark must start at 0 —
	// re-pushing seq 1 is an idempotent dedup; losing seq 2 is not.
	seedLocalEvents(t, st, [][2]int64{{1, 100}, {2, 50}})
	if err := st.AdvanceHubCursor(ctx, "push:"+hubID, 100); err != nil {
		t.Fatal(err)
	}
	seq, err := st.PushSeqCursor(ctx, hubID)
	if err != nil {
		t.Fatal(err)
	}
	if seq != 0 {
		t.Fatalf("push cursor = %d, want 0 (legacy HLC watermark must not be trusted)", seq)
	}
	pending, err := st.LocalPendingEventsBySeq(ctx, seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending = %+v, want both events (incl. the stranded regressed-HLC seq 2)", pending)
	}
	if err := st.AdvancePushSeqCursor(ctx, hubID, 2); err != nil {
		t.Fatal(err)
	}
	seq, err = st.PushSeqCursor(ctx, hubID)
	if err != nil {
		t.Fatal(err)
	}
	if seq != 2 {
		t.Fatalf("push cursor after advance = %d, want 2", seq)
	}
}

func TestPushSeqCursorFreshStoreIsZero(t *testing.T) {
	ctx := context.Background()
	st := openCursorTestStore(t)
	if _, err := st.EnsureDevice(ctx, "test-host"); err != nil {
		t.Fatal(err)
	}
	seq, err := st.PushSeqCursor(ctx, "file:/tmp/hub.json")
	if err != nil {
		t.Fatal(err)
	}
	if seq != 0 {
		t.Fatalf("fresh push cursor = %d, want 0 (no legacy watermark, no backfill row)", seq)
	}
}
