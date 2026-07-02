package state

import (
	"context"
	"path/filepath"
	"testing"
)

func newTestStoreWithWorkspace(t *testing.T) *Store {
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

func TestRecordKeyEpochRoundTripsViaHeldKeys(t *testing.T) {
	ctx := context.Background()
	st := newTestStoreWithWorkspace(t)

	if err := st.RecordKeyEpoch(ctx, 1, "0123456789abcdef", "self"); err != nil {
		t.Fatal(err)
	}
	keys, err := st.HeldKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("HeldKeys = %+v, want 1 entry", keys)
	}
	want := HeldKey{Epoch: 1, KID: "0123456789abcdef", Origin: "self"}
	if keys[0] != want {
		t.Fatalf("HeldKeys[0] = %+v, want %+v", keys[0], want)
	}
}

func TestRecordKeyEpochTwoKidsSameEpochCoexist(t *testing.T) {
	ctx := context.Background()
	st := newTestStoreWithWorkspace(t)

	if err := st.RecordKeyEpoch(ctx, 2, "aaaaaaaaaaaaaaaa", "self"); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordKeyEpoch(ctx, 2, "bbbbbbbbbbbbbbbb", "grant"); err != nil {
		t.Fatal(err)
	}
	keys, err := st.HeldKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("HeldKeys = %+v, want 2 entries", keys)
	}

	epochs, err := st.HeldKeyEpochs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(epochs) != 1 || epochs[0] != 2 {
		t.Fatalf("HeldKeyEpochs = %v, want [2]", epochs)
	}
}

func TestRecordKeyEpochIdempotentOnSameEpochKid(t *testing.T) {
	ctx := context.Background()
	st := newTestStoreWithWorkspace(t)

	for i := 0; i < 2; i++ {
		if err := st.RecordKeyEpoch(ctx, 3, "cccccccccccccccc", "self"); err != nil {
			t.Fatalf("RecordKeyEpoch call %d: %v", i, err)
		}
	}
	keys, err := st.HeldKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("HeldKeys = %+v, want 1 entry after idempotent re-record", keys)
	}
}

func TestUpdateKeyKidUpgradesLegacyRowPreservingOrigin(t *testing.T) {
	ctx := context.Background()
	st := newTestStoreWithWorkspace(t)

	// Simulate a pre-kid row backfilled by the migration: kid = "", origin = "legacy".
	if err := st.RecordKeyEpoch(ctx, 4, "", "legacy"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateKeyKid(ctx, 4, "dddddddddddddddd"); err != nil {
		t.Fatal(err)
	}
	keys, err := st.HeldKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("HeldKeys = %+v, want 1 entry after upgrade", keys)
	}
	want := HeldKey{Epoch: 4, KID: "dddddddddddddddd", Origin: "legacy"}
	if keys[0] != want {
		t.Fatalf("HeldKeys[0] = %+v, want %+v (origin preserved)", keys[0], want)
	}
}

func TestUpdateKeyKidNoOpWhenTargetAlreadyExists(t *testing.T) {
	ctx := context.Background()
	st := newTestStoreWithWorkspace(t)

	if err := st.RecordKeyEpoch(ctx, 5, "", "legacy"); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordKeyEpoch(ctx, 5, "eeeeeeeeeeeeeeee", "self"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateKeyKid(ctx, 5, "eeeeeeeeeeeeeeee"); err != nil {
		t.Fatal(err)
	}
	keys, err := st.HeldKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("HeldKeys = %+v, want 1 entry (legacy row deleted, existing target untouched)", keys)
	}
	// The pre-existing "self" origin must not have been overwritten by the
	// legacy row's origin.
	want := HeldKey{Epoch: 5, KID: "eeeeeeeeeeeeeeee", Origin: "self"}
	if keys[0] != want {
		t.Fatalf("HeldKeys[0] = %+v, want %+v", keys[0], want)
	}
}

func TestUpdateKeyKidNoLegacyRowIsNoOp(t *testing.T) {
	ctx := context.Background()
	st := newTestStoreWithWorkspace(t)

	if err := st.UpdateKeyKid(ctx, 6, "ffffffffffffffff"); err != nil {
		t.Fatal(err)
	}
	keys, err := st.HeldKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Fatalf("HeldKeys = %+v, want none", keys)
	}
}

func TestRecordKeyGrantAcceptsKidAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st := newTestStoreWithWorkspace(t)

	for i := 0; i < 2; i++ {
		if err := st.RecordKeyGrant(ctx, 7, "1111111111111111", "age1recipient", "evt-1", 100, "dev_1"); err != nil {
			t.Fatalf("RecordKeyGrant call %d: %v", i, err)
		}
	}
	var count int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM workspace_key_grants WHERE epoch = 7`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("workspace_key_grants rows = %d, want 1", count)
	}
	var kid string
	if err := st.db.QueryRowContext(ctx, `SELECT kid FROM workspace_key_grants WHERE epoch = 7`).Scan(&kid); err != nil {
		t.Fatal(err)
	}
	if kid != "1111111111111111" {
		t.Fatalf("stored kid = %q, want 1111111111111111", kid)
	}
}

func TestRecordKeyGrantTxAcceptsKidAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st := newTestStoreWithWorkspace(t)

	event := Event{ID: "evt-2", HLC: 200, DeviceID: "dev_2"}
	for i := 0; i < 2; i++ {
		if err := st.WithTx(ctx, func(tx *Tx) error {
			return tx.RecordKeyGrantTx(ctx, 8, "2222222222222222", "age1recipient", event)
		}); err != nil {
			t.Fatalf("RecordKeyGrantTx call %d: %v", i, err)
		}
	}
	var count int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM workspace_key_grants WHERE epoch = 8`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("workspace_key_grants rows = %d, want 1", count)
	}
	var kid string
	if err := st.db.QueryRowContext(ctx, `SELECT kid FROM workspace_key_grants WHERE epoch = 8`).Scan(&kid); err != nil {
		t.Fatal(err)
	}
	if kid != "2222222222222222" {
		t.Fatalf("stored kid = %q, want 2222222222222222", kid)
	}
}
