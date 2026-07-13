package state

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestReaderPoolNotBlockedByWriteTxn proves concurrent SELECTs use the
// read-only pool and complete while a write transaction holds the single
// writer connection (P4-SYNC-07). If Summary were still routed through the
// MaxOpenConns(1) writer, this read would block until the write txn ends and
// miss the 2s deadline.
func TestReaderPoolNotBlockedByWriteTxn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(t.Context(), "ws", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureDevice(t.Context(), "device"); err != nil {
		t.Fatal(err)
	}
	if st.readDB == nil {
		t.Fatal("Open did not configure readDB")
	}

	// Hold the writer connection open inside a transaction.
	tx, err := st.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin write txn: %v", err)
	}
	// Touch local_meta so the writer is busy with a real statement.
	if _, err := tx.ExecContext(t.Context(), `
INSERT INTO local_meta (key, value, updated_at)
VALUES ('p4-sync-07-hold', '1', ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at;
`, timestampNow()); err != nil {
		_ = tx.Rollback()
		t.Fatalf("write inside held txn: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	release := make(chan struct{})
	go func() {
		defer wg.Done()
		<-release
		if err := tx.Commit(); err != nil {
			t.Errorf("commit held write txn: %v", err)
		}
	}()

	readCtx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	summary, err := st.Summary(readCtx)
	if err != nil {
		close(release)
		wg.Wait()
		t.Fatalf("Summary while write txn held: %v (would timeout if routed through single writer)", err)
	}
	if summary.WorkspaceName != "ws" {
		t.Fatalf("Summary.WorkspaceName = %q, want ws", summary.WorkspaceName)
	}

	close(release)
	wg.Wait()
}
