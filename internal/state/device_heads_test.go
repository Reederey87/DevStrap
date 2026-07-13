package state

import (
	"context"
	"testing"

	"github.com/Reederey87/DevStrap/internal/fold"
)

func rawInsertEvent(t *testing.T, st *Store, ws, dev string, seq int64, contentHash string) {
	t.Helper()
	_, err := st.db.ExecContext(context.Background(), `
INSERT INTO events (id, workspace_id, device_id, seq, hlc, type, payload_json, content_hash, created_at)
VALUES (?, ?, ?, ?, ?, 'project.added', '{}', ?, '2026-01-01T00:00:00Z');
`, "evt_"+dev+itoaState(seq), ws, dev, seq, seq<<hlcLogicalBits, contentHash)
	if err != nil {
		t.Fatal(err)
	}
}

func itoaState(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestDeviceGapLocallyDeclined covers the three signals that classify a fold gap
// as a LOCAL decline (P4-SYNC-05 defect fix) rather than hub withholding: a
// skipped-event row at the exact slot, any open key-grant wait (coarse — not
// seq-attributed), and an open quarantine conflict naming the device (with a
// matching seq, or none at all, e.g. a skew quarantine).
func TestDeviceGapLocallyDeclined(t *testing.T) {
	ctx := context.Background()
	st, _ := newAnchorStore(t)
	const dev = "dev_peer"
	if err := st.EnsureRemoteDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	quarantineTypes := []string{"untrustworthy_remote_time", "event_hash_chain_break", "event_verification_failure"}

	// Baseline: no local signal at all.
	if declined, err := st.DeviceGapLocallyDeclined(ctx, dev, 8, quarantineTypes); err != nil || declined {
		t.Fatalf("baseline: declined=%v err=%v, want false/nil", declined, err)
	}

	// (1) skipped-event row at the exact slot.
	if _, err := st.NoteSkippedEvent(ctx, Event{ID: "evt_skip", DeviceID: dev, Seq: 8, HLC: 8 << hlcLogicalBits}, "unknown_version"); err != nil {
		t.Fatal(err)
	}
	if declined, err := st.DeviceGapLocallyDeclined(ctx, dev, 8, quarantineTypes); err != nil || !declined {
		t.Fatalf("skip slot: declined=%v err=%v, want true", declined, err)
	}
	// A DIFFERENT slot with no other signal is not declined.
	if declined, err := st.DeviceGapLocallyDeclined(ctx, dev, 9, quarantineTypes); err != nil || declined {
		t.Fatalf("other slot: declined=%v err=%v, want false", declined, err)
	}

	// (2) a quarantine conflict carrying NO seq (a skew quarantine) matches by
	// device_id alone, for any gap slot.
	if err := st.InsertConflict(ctx, "", "untrustworthy_remote_time", `{"event_id":"evt_skew","device_id":"dev_peer","hlc":5}`); err != nil {
		t.Fatal(err)
	}
	if declined, err := st.DeviceGapLocallyDeclined(ctx, dev, 9, quarantineTypes); err != nil || !declined {
		t.Fatalf("skew conflict (no seq): declined=%v err=%v, want true", declined, err)
	}
	// A conflict for a DIFFERENT device does not match.
	if declined, err := st.DeviceGapLocallyDeclined(ctx, "dev_other", 9, quarantineTypes); err != nil || declined {
		t.Fatalf("other device: declined=%v err=%v, want false", declined, err)
	}
}

// TestResolveOmissionConflictsForDevice: kind-filtered resolution clears a stale
// withheld_tail without touching a genuine fork, and an empty kind clears both.
func TestResolveOmissionConflictsForDevice(t *testing.T) {
	ctx := context.Background()
	st, _ := newAnchorStore(t)
	insert := func(dev, kind string) {
		t.Helper()
		if err := st.InsertConflict(ctx, "", "event_omission",
			`{"device_id":"`+dev+`","kind":"`+kind+`","local_seq":2}`); err != nil {
			t.Fatal(err)
		}
	}
	countOpen := func() int {
		t.Helper()
		open, err := st.OpenConflictsByType(ctx, "event_omission")
		if err != nil {
			t.Fatal(err)
		}
		return len(open)
	}
	insert("dev_a", "withheld_tail")
	insert("dev_a", "fork")
	insert("dev_b", "withheld_tail")
	if countOpen() != 3 {
		t.Fatalf("setup: want 3 open, got %d", countOpen())
	}
	// Kind-filtered: clear only dev_a's withheld_tail.
	if err := st.ResolveOmissionConflictsForDevice(ctx, "event_omission", "dev_a", "withheld_tail", `{"action":"auto"}`); err != nil {
		t.Fatal(err)
	}
	if countOpen() != 2 {
		t.Fatalf("after kind-filtered resolve: want 2 open (dev_a fork + dev_b), got %d", countOpen())
	}
	// Empty kind: clear everything remaining for dev_a (its fork).
	if err := st.ResolveOmissionConflictsForDevice(ctx, "event_omission", "dev_a", "", `{"action":"auto"}`); err != nil {
		t.Fatal(err)
	}
	if countOpen() != 1 {
		t.Fatalf("after all-kinds resolve for dev_a: want 1 open (dev_b), got %d", countOpen())
	}
}

// TestDeviceFoldFromSeqOne: with no anchor, the fold seeds from FoldSeed and
// walks the contiguous stream; a gap stops the walk at the last contiguous seq.
func TestDeviceFoldFromSeqOne(t *testing.T) {
	ctx := context.Background()
	st, ws := newAnchorStore(t)
	const dev = "dev_peer"
	if err := st.EnsureRemoteDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	rawInsertEvent(t, st, ws, dev, 1, "sha256:c1")
	rawInsertEvent(t, st, ws, dev, 2, "sha256:c2")
	rawInsertEvent(t, st, ws, dev, 4, "sha256:c4") // gap at 3

	reached, got, seeded, err := st.DeviceFold(ctx, dev, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !seeded || reached != 2 {
		t.Fatalf("reached=%d seeded=%v, want reached=2 seeded=true (gap after 2)", reached, seeded)
	}
	want := fold.Encode(fold.Step(fold.Step(fold.Seed(ws, dev), 1, "sha256:c1"), 2, "sha256:c2"))
	if got != want {
		t.Fatalf("fold mismatch:\n got %s\nwant %s", got, want)
	}
}

// TestDeviceFoldSeedsFromAnchor: a snapshot-bootstrapped device holds no events
// below the floor; the fold seeds from the imported anchor's folded hash and
// folds the post-floor events forward. This is the compaction-survival path.
func TestDeviceFoldSeedsFromAnchor(t *testing.T) {
	ctx := context.Background()
	st, ws := newAnchorStore(t)
	const dev = "dev_boot"
	if err := st.EnsureRemoteDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	// Anchor at seq 4 carrying an arbitrary but fixed fold seed.
	anchorFold := fold.Encode(fold.Step(fold.Seed(ws, dev), 4, "sha256:covered"))
	if err := st.WithTx(ctx, func(tx *Tx) error {
		return tx.UpsertChainAnchor(ctx, dev, 4, "sha256:anchor4", anchorFold, 900, "snapsha")
	}); err != nil {
		t.Fatal(err)
	}
	rawInsertEvent(t, st, ws, dev, 5, "sha256:c5")
	rawInsertEvent(t, st, ws, dev, 6, "sha256:c6")

	reached, got, seeded, err := st.DeviceFold(ctx, dev, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !seeded || reached != 6 {
		t.Fatalf("reached=%d seeded=%v, want reached=6 seeded=true", reached, seeded)
	}
	seed, _, _ := fold.Decode(anchorFold)
	want := fold.Encode(fold.Step(fold.Step(seed, 5, "sha256:c5"), 6, "sha256:c6"))
	if got != want {
		t.Fatalf("anchor-seeded fold mismatch:\n got %s\nwant %s", got, want)
	}

	// Bounded walk: fold only up to seq 5.
	reached5, got5, _, err := st.DeviceFold(ctx, dev, 5)
	if err != nil {
		t.Fatal(err)
	}
	want5 := fold.Encode(fold.Step(seed, 5, "sha256:c5"))
	if reached5 != 5 || got5 != want5 {
		t.Fatalf("bounded fold: reached=%d got=%s, want reached=5 got=%s", reached5, got5, want5)
	}
}

// TestDeviceFoldUnseededWhenPrefixMissing: no anchor AND the stream does not
// start at seq 1 (early events neither held nor covered) → unseeded, so callers
// skip verification fail-safe.
func TestDeviceFoldUnseededWhenPrefixMissing(t *testing.T) {
	ctx := context.Background()
	st, ws := newAnchorStore(t)
	const dev = "dev_hole"
	if err := st.EnsureRemoteDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	rawInsertEvent(t, st, ws, dev, 3, "sha256:c3") // starts at 3, no anchor
	rawInsertEvent(t, st, ws, dev, 4, "sha256:c4")

	_, _, seeded, err := st.DeviceFold(ctx, dev, 0)
	if err != nil {
		t.Fatal(err)
	}
	if seeded {
		t.Fatal("a stream starting at seq 3 with no anchor must be unseeded")
	}
}
