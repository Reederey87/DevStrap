package state

import (
	"context"
	"path/filepath"
	"testing"
)

// P6-SYNC-02: sync_skipped_events is the durable record of pull-dropped
// events. first_seen_at must be stable across re-sightings (the grace clock
// for the unknown-version class), and applying the event clears its records.

func openSkipTestStore(t *testing.T) *Store {
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

func TestNoteSkippedEventFirstSeenIsStable(t *testing.T) {
	ctx := context.Background()
	st := openSkipTestStore(t)
	ev := Event{ID: "evt_x", DeviceID: "dev_a", Seq: 3, HLC: 30}
	first, err := st.NoteSkippedEvent(ctx, ev, "unknown-envelope-version")
	if err != nil {
		t.Fatal(err)
	}
	again, err := st.NoteSkippedEvent(ctx, ev, "unknown-envelope-version")
	if err != nil {
		t.Fatal(err)
	}
	if !again.Equal(first) {
		t.Fatalf("re-sighting moved first-seen: %v -> %v", first, again)
	}
	open, err := st.OpenSkippedEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].EventID != "evt_x" || open[0].Reason != "unknown-envelope-version" || open[0].DeviceID != "dev_a" {
		t.Fatalf("open = %+v, want one evt_x record", open)
	}
}

func TestClearSkippedEventTxRemovesAllReasons(t *testing.T) {
	ctx := context.Background()
	st := openSkipTestStore(t)
	ev := Event{ID: "evt_x", DeviceID: "dev_a", Seq: 3, HLC: 30}
	if _, err := st.NoteSkippedEvent(ctx, ev, "unknown-envelope-version"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.NoteSkippedEvent(ctx, ev, "plaintext-anti-downgrade"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.NoteSkippedEvent(ctx, Event{ID: "evt_other", DeviceID: "dev_b", Seq: 1, HLC: 5}, "retired-enc-v1"); err != nil {
		t.Fatal(err)
	}
	if err := st.WithTx(ctx, func(tx *Tx) error {
		return tx.ClearSkippedEventTx(ctx, "evt_x")
	}); err != nil {
		t.Fatal(err)
	}
	open, err := st.OpenSkippedEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].EventID != "evt_other" {
		t.Fatalf("open after clear = %+v, want only evt_other", open)
	}
	// Idempotent: clearing again is a no-op.
	if err := st.WithTx(ctx, func(tx *Tx) error {
		return tx.ClearSkippedEventTx(ctx, "evt_x")
	}); err != nil {
		t.Fatal(err)
	}
}
