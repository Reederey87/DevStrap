// wck_rotation_test.go pins the state-level owed-rotation helpers used by the
// sync apply path (P7-SYNC-04): the epoch>0 guard, the transactional set, and
// the storm-guard that leaves an existing marker untouched.
package state

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func newWCKStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(ctx, "test", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	return st
}

func readOwedMarker(t *testing.T, st *Store) (WCKRotationPendingRecord, bool) {
	t.Helper()
	raw, ok, err := st.GetLocalMeta(context.Background(), WCKRotationPendingMetaKey)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return WCKRotationPendingRecord{}, false
	}
	var rec WCKRotationPendingRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		t.Fatalf("owed marker %q does not parse: %v", raw, err)
	}
	return rec, true
}

// TestSetWCKRotationPendingTxArmsAtEpoch: a fresh call at epoch>0 writes the
// marker with that epoch and a non-zero Since.
func TestSetWCKRotationPendingTxArmsAtEpoch(t *testing.T) {
	ctx := context.Background()
	st := newWCKStore(t)
	if err := st.WithTx(ctx, func(tx *Tx) error {
		return tx.SetWCKRotationPendingTx(ctx, 5)
	}); err != nil {
		t.Fatal(err)
	}
	rec, ok := readOwedMarker(t, st)
	if !ok {
		t.Fatal("SetWCKRotationPendingTx(5) armed no marker")
	}
	if rec.Epoch != 5 || rec.Since.IsZero() {
		t.Fatalf("marker=%+v, want epoch 5 and non-zero Since", rec)
	}
}

// TestSetWCKRotationPendingTxEpochZeroIsNoop: epoch 0 (keyless) never arms.
func TestSetWCKRotationPendingTxEpochZeroIsNoop(t *testing.T) {
	ctx := context.Background()
	st := newWCKStore(t)
	if err := st.WithTx(ctx, func(tx *Tx) error {
		return tx.SetWCKRotationPendingTx(ctx, 0)
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := readOwedMarker(t, st); ok {
		t.Fatal("epoch 0 armed a marker; want no-op")
	}
}

// TestSetWCKRotationPendingTxStormGuard: an existing marker is left untouched,
// preserving its Since across a later arm at a different epoch.
func TestSetWCKRotationPendingTxStormGuard(t *testing.T) {
	ctx := context.Background()
	st := newWCKStore(t)
	if err := st.WithTx(ctx, func(tx *Tx) error {
		return tx.SetWCKRotationPendingTx(ctx, 2)
	}); err != nil {
		t.Fatal(err)
	}
	first, _ := readOwedMarker(t, st)

	time.Sleep(2 * time.Millisecond)
	if err := st.WithTx(ctx, func(tx *Tx) error {
		return tx.SetWCKRotationPendingTx(ctx, 9)
	}); err != nil {
		t.Fatal(err)
	}
	second, _ := readOwedMarker(t, st)
	if !second.Since.Equal(first.Since) || second.Epoch != first.Epoch {
		t.Fatalf("marker changed %+v -> %+v, want the original preserved (storm-guard)", first, second)
	}
}

// TestCurrentKeyEpochTxMatchesStore: the transactional epoch read agrees with
// the non-transactional CurrentKeyEpoch (0 when none held, MAX otherwise).
func TestCurrentKeyEpochTxMatchesStore(t *testing.T) {
	ctx := context.Background()
	st := newWCKStore(t)

	var txEpoch int64
	if err := st.WithTx(ctx, func(tx *Tx) error {
		var err error
		txEpoch, err = tx.CurrentKeyEpochTx(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if txEpoch != 0 {
		t.Fatalf("CurrentKeyEpochTx with no keys=%d, want 0", txEpoch)
	}

	if err := st.RecordKeyEpoch(ctx, 4, "kid-a", "self"); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordKeyEpoch(ctx, 7, "kid-b", "self"); err != nil {
		t.Fatal(err)
	}
	if err := st.WithTx(ctx, func(tx *Tx) error {
		var err error
		txEpoch, err = tx.CurrentKeyEpochTx(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	storeEpoch, err := st.CurrentKeyEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if txEpoch != 7 || txEpoch != storeEpoch {
		t.Fatalf("CurrentKeyEpochTx=%d, CurrentKeyEpoch=%d, want both 7", txEpoch, storeEpoch)
	}
}
