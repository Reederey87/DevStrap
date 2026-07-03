package state

import (
	"context"
	"path/filepath"
	"testing"
)

// P6-SEC-03: key_grant_waits gives the missing-grant grace window a stable
// start. first_seen_at must never move on re-sighting (a re-pull cannot
// restart the window), the epoch clock must be shared across kids (a hostile
// hub relabeling the unauthenticated kid hint cannot mint fresh windows), and
// RecordKeyEpoch must clear satisfied waits.

func openKeyGrantWaitTestStore(t *testing.T) *Store {
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

func TestNoteMissingKeyGrantFirstSeenIsStable(t *testing.T) {
	ctx := context.Background()
	st := openKeyGrantWaitTestStore(t)
	first, err := st.NoteMissingKeyGrant(ctx, 5, "")
	if err != nil {
		t.Fatal(err)
	}
	again, err := st.NoteMissingKeyGrant(ctx, 5, "")
	if err != nil {
		t.Fatal(err)
	}
	if !again.Equal(first) {
		t.Fatalf("re-sighting moved first-seen: %v -> %v", first, again)
	}
}

func TestNoteMissingKeyGrantKidChurnSharesEpochClock(t *testing.T) {
	ctx := context.Background()
	st := openKeyGrantWaitTestStore(t)
	first, err := st.NoteMissingKeyGrant(ctx, 3, "kid-aaaa")
	if err != nil {
		t.Fatal(err)
	}
	// A hostile hub relabels the same carrier with a fresh kid: the epoch's
	// clock must keep running from the FIRST sighting, not restart per label.
	relabeled, err := st.NoteMissingKeyGrant(ctx, 3, "kid-bbbb")
	if err != nil {
		t.Fatal(err)
	}
	if !relabeled.Equal(first) {
		t.Fatalf("kid relabel restarted the epoch clock: %v -> %v", first, relabeled)
	}
	// A DIFFERENT epoch gets its own clock.
	other, err := st.NoteMissingKeyGrant(ctx, 4, "")
	if err != nil {
		t.Fatal(err)
	}
	if other.Before(first) {
		t.Fatalf("epoch-4 clock %v predates epoch-3 first sighting %v", other, first)
	}
	waits, err := st.OpenKeyGrantWaits(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(waits) != 3 {
		t.Fatalf("OpenKeyGrantWaits = %+v, want 3 rows (two kids at epoch 3, one at epoch 4)", waits)
	}
}

func TestRecordKeyEpochClearsSatisfiedWaits(t *testing.T) {
	ctx := context.Background()
	st := openKeyGrantWaitTestStore(t)
	if _, err := st.NoteMissingKeyGrant(ctx, 2, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.NoteMissingKeyGrant(ctx, 2, "kid-fleet"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.NoteMissingKeyGrant(ctx, 2, "kid-other"); err != nil {
		t.Fatal(err)
	}
	// Holding (2, kid-fleet) clears the epoch-level wait and the matching kid
	// wait, but NOT the wait for a different kid (still undecryptable).
	if err := st.RecordKeyEpoch(ctx, 2, "kid-fleet", "grant"); err != nil {
		t.Fatal(err)
	}
	waits, err := st.OpenKeyGrantWaits(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(waits) != 1 || waits[0].Epoch != 2 || waits[0].KID != "kid-other" {
		t.Fatalf("OpenKeyGrantWaits = %+v, want only the kid-other wait left open", waits)
	}
}
