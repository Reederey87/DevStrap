package state

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func newAnchorStore(t *testing.T) (*Store, string) {
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
	ws, err := st.WorkspaceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return st, ws
}

// TestPrevEventHashFallsBackToChainAnchor: a snapshot-bootstrapped device has no
// event rows below the retention floor, so the prev-hash of the first post-floor
// event resolves against the imported chain anchor. A matching anchor passes;
// a mismatch is an ErrEventHashChain break.
func TestPrevEventHashFallsBackToChainAnchor(t *testing.T) {
	ctx := context.Background()
	st, ws := newAnchorStore(t)

	const dev = "dev_remote"
	const anchorHash = "sha256:anchor4"
	if err := st.WithTx(ctx, func(tx *Tx) error {
		return tx.UpsertChainAnchor(ctx, dev, 4, anchorHash, "", 900, "snapsha")
	}); err != nil {
		t.Fatal(err)
	}

	// First post-floor event (Seq 5) whose predecessor (Seq 4) is only the anchor.
	good := Event{WorkspaceID: ws, DeviceID: dev, Seq: 5, HLC: 1000, PrevEventHash: anchorHash}
	if err := validatePrevEventHash(ctx, st.db, good); err != nil {
		t.Fatalf("anchor fallback should validate the first post-floor event: %v", err)
	}

	bad := good
	bad.PrevEventHash = "sha256:wrong"
	if err := validatePrevEventHash(ctx, st.db, bad); !errors.Is(err, ErrEventHashChain) {
		t.Fatalf("mismatched prev-hash against anchor: got %v, want ErrEventHashChain", err)
	}

	// A device with no anchor and no seq-1 event still reports a hash-chain break.
	orphan := Event{WorkspaceID: ws, DeviceID: "dev_unknown", Seq: 5, HLC: 1000, PrevEventHash: anchorHash}
	if err := validatePrevEventHash(ctx, st.db, orphan); !errors.Is(err, ErrEventHashChain) {
		t.Fatalf("orphaned prev-hash with no anchor: got %v, want ErrEventHashChain", err)
	}
}

// TestUpsertChainAnchorKeepsMaxSeq: a later snapshot's higher anchor_seq wins; a
// stale re-import with a lower seq never regresses the anchor.
func TestUpsertChainAnchorKeepsMaxSeq(t *testing.T) {
	ctx := context.Background()
	st, _ := newAnchorStore(t)
	const dev = "dev_a"

	read := func() (int64, string) {
		var seq int64
		var hash string
		if err := st.db.QueryRowContext(ctx, `
SELECT anchor_seq, anchor_content_hash FROM sync_chain_anchors WHERE device_id = ?;
`, dev).Scan(&seq, &hash); err != nil {
			t.Fatal(err)
		}
		return seq, hash
	}

	upsert := func(seq int64, hash string) {
		if err := st.WithTx(ctx, func(tx *Tx) error {
			return tx.UpsertChainAnchor(ctx, dev, seq, hash, "", seq*10, "sha")
		}); err != nil {
			t.Fatal(err)
		}
	}

	upsert(4, "h4")
	if seq, hash := read(); seq != 4 || hash != "h4" {
		t.Fatalf("after first upsert: seq=%d hash=%s", seq, hash)
	}
	// Lower seq must not regress.
	upsert(2, "h2")
	if seq, hash := read(); seq != 4 || hash != "h4" {
		t.Fatalf("lower seq regressed the anchor: seq=%d hash=%s", seq, hash)
	}
	// Higher seq wins.
	upsert(7, "h7")
	if seq, hash := read(); seq != 7 || hash != "h7" {
		t.Fatalf("higher seq did not win: seq=%d hash=%s", seq, hash)
	}
}
