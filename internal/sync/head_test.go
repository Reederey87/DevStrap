package sync

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/fold"
	"github.com/Reederey87/DevStrap/internal/state"
)

// peerHeadFixture builds device B's store with an approved peer "device-peer",
// applies the first `received` of `total` signed peer events, and returns the
// full ordered event list plus the peer's signing identity so the caller can
// craft the peer's signed head (ack) at any seq/fold it likes.
func peerHeadFixture(t *testing.T, total, received int) (*state.Store, state.Device, []state.Event, devicekeys.SigningIdentity) {
	t.Helper()
	ctx := context.Background()
	st, self := newSyncStore(t)
	signing := addRemoteDeviceForApplyTest(t, st, "device-peer", "approved")

	events := make([]state.Event, total)
	for i := 0; i < total; i++ {
		seq := int64(i + 1)
		hlc := (1000 + seq) << hlcLogicalBits
		events[i] = signedProjectEvent(t, signing, "device-peer", seq, hlc,
			"work/acme/p"+itoa(seq), "github.com/acme/p"+itoa(seq))
	}
	if received > 0 {
		if _, _, err := ApplyEventsWithStats(ctx, st, events[:received], nil); err != nil {
			t.Fatalf("apply received peer events: %v", err)
		}
	}
	return st, self, events, signing
}

func itoa(n int64) string {
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

// peerFoldAt folds the peer's stream (as device B seeds it: from FoldSeed at seq
// 0) up to `upto`, returning the encoded head fold the peer would sign.
func peerFoldAt(t *testing.T, st *state.Store, events []state.Event, upto int) string {
	t.Helper()
	ws, err := st.WorkspaceID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f := fold.Seed(ws, "device-peer")
	for i := 0; i < upto; i++ {
		f = fold.Step(f, int64(i+1), events[i].ContentHash)
	}
	return fold.Encode(f)
}

// putPeerHead signs and publishes the peer's head (ack) at the given seq/fold.
func putPeerHead(t *testing.T, st *state.Store, hub Hub, signing devicekeys.SigningIdentity, seq int64, foldHex string) {
	t.Helper()
	ws, err := st.WorkspaceID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m := AckMarker{
		Cursor:           map[string]int64{},
		DeviceID:         "device-peer",
		FoldedHash:       foldHex,
		HLCWatermark:     seq << hlcLogicalBits,
		ProducedAt:       seq << hlcLogicalBits,
		PushedThroughSeq: seq,
		WorkspaceID:      ws,
	}
	if err := SignAckMarker(&m, signing.Private); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := hub.PutAck(context.Background(), "device-peer", raw); err != nil {
		t.Fatal(err)
	}
}

func openOmissionConflicts(t *testing.T, st *state.Store) []state.Conflict {
	t.Helper()
	all, err := st.OpenConflicts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []state.Conflict
	for _, c := range all {
		if c.Type == ConflictEventOmission {
			out = append(out, c)
		}
	}
	return out
}

// TestVerifyPeerHeadsDetectsWithheldTail is the core omission-detection
// property (P4-SYNC-05): a hub serves the peer's SIGNED head committing to seq
// 10 but withholds events 8..10, so device B holds only 1..7. B must detect the
// gap via the head — not merely fail to converge silently. The first cycle is
// the in-flight-race grace (no alarm); the second raises the alarm.
func TestVerifyPeerHeadsDetectsWithheldTail(t *testing.T) {
	ctx := context.Background()
	st, self, events, signing := peerHeadFixture(t, 10, 7) // B received only 1..7
	hub := testFileHub(t)

	// The peer's head commits to the full seq 10 with the true fold@10, even
	// though the hub withheld events 8..10 from B.
	putPeerHead(t, st, hub, signing, 10, peerFoldAt(t, st, events, 10))

	// Cycle 1: one-cycle grace absorbs a legitimate in-flight race.
	n, err := VerifyPeerHeads(ctx, st, hub, workspaceOf(t, st), self.ID)
	if err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	if n != 0 || len(openOmissionConflicts(t, st)) != 0 {
		t.Fatalf("cycle 1 should not alarm (grace); got n=%d conflicts=%d", n, len(openOmissionConflicts(t, st)))
	}

	// Cycle 2: the hub is still withholding — this is the omission signal.
	n, err = VerifyPeerHeads(ctx, st, hub, workspaceOf(t, st), self.ID)
	if err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	if n != 1 {
		t.Fatalf("cycle 2 should detect 1 omission, got %d", n)
	}
	conflicts := openOmissionConflicts(t, st)
	if len(conflicts) != 1 {
		t.Fatalf("want 1 open omission conflict, got %d", len(conflicts))
	}
	dev, kind, localSeq, err := ParseOmissionConflictDetails(conflicts[0].DetailsJSON)
	if err != nil {
		t.Fatal(err)
	}
	if dev != "device-peer" || kind != OmissionKindWithheldTail || localSeq != 7 {
		t.Fatalf("details = (%s, %s, %d), want (device-peer, withheld_tail, 7)", dev, kind, localSeq)
	}

	// Idempotent: a third cycle re-alarms but dedups to the same single row.
	if _, err := VerifyPeerHeads(ctx, st, hub, workspaceOf(t, st), self.ID); err != nil {
		t.Fatal(err)
	}
	if got := len(openOmissionConflicts(t, st)); got != 1 {
		t.Fatalf("omission conflict should dedup, got %d rows", got)
	}
}

// TestVerifyPeerHeadsSuppressesLocallyDeclinedGap is the H1(b) property: when the
// FIRST missing seq in a peer's prefix is one THIS device durably declined (here
// a sync_skipped_events row for that slot), the shortfall is a LOCAL gap — not
// hub withholding — so no `withheld_tail` alarm fires against the honest peer,
// however many cycles run. (A cross-epoch key-grant grace and a consumed
// skew/hash-chain quarantine reach the same suppression via the other branches
// of DeviceGapLocallyDeclined.)
func TestVerifyPeerHeadsSuppressesLocallyDeclinedGap(t *testing.T) {
	ctx := context.Background()
	st, self, events, signing := peerHeadFixture(t, 10, 7) // B holds 1..7
	hub := testFileHub(t)

	// Event 8 (the first missing slot) is durably declined by THIS device — it
	// was deferred at the hub-decrypt boundary, not withheld by the hub.
	if _, err := st.NoteSkippedEvent(ctx, events[7], "unknown_envelope_version"); err != nil {
		t.Fatalf("note skipped event: %v", err)
	}

	// The peer's head honestly commits to the full seq 10.
	putPeerHead(t, st, hub, signing, 10, peerFoldAt(t, st, events, 10))

	// Even past the one-cycle grace, no omission alarm: the gap is local.
	for cycle := 0; cycle < 3; cycle++ {
		n, err := VerifyPeerHeads(ctx, st, hub, workspaceOf(t, st), self.ID)
		if err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
		if n != 0 {
			t.Fatalf("cycle %d: a locally-declined gap must not alarm as withheld_tail", cycle)
		}
	}
	if got := len(openOmissionConflicts(t, st)); got != 0 {
		t.Fatalf("locally-declined gap must not create an omission conflict, got %d", got)
	}
}

// TestVerifyPeerHeadsResolvesOnCatchUp is the H1(a) recovery property: an
// event_omission conflict raised for a withheld tail RESOLVES once the peer's
// events are backfilled and this device's independent fold catches up to (and
// matches) the promised head. Without this, the alarm — re-created on every pull
// while the gap persisted — would never clear and would permanently block
// `hub gc` even after the gap was healed.
func TestVerifyPeerHeadsResolvesOnCatchUp(t *testing.T) {
	ctx := context.Background()
	st, self, events, signing := peerHeadFixture(t, 10, 7) // B holds 1..7; 8..10 withheld
	hub := testFileHub(t)
	putPeerHead(t, st, hub, signing, 10, peerFoldAt(t, st, events, 10))

	// Two cycles raise the withheld_tail alarm (cycle 1 is grace).
	for cycle := 0; cycle < 2; cycle++ {
		if _, err := VerifyPeerHeads(ctx, st, hub, workspaceOf(t, st), self.ID); err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
	}
	if got := len(openOmissionConflicts(t, st)); got != 1 {
		t.Fatalf("want 1 open omission conflict after withhold, got %d", got)
	}

	// The hub stops withholding: events 8..10 arrive and apply.
	if _, _, err := ApplyEventsWithStats(ctx, st, events[7:], nil); err != nil {
		t.Fatalf("backfill withheld tail: %v", err)
	}

	// The next verify sees the full, matching prefix and resolves the alarm.
	n, err := VerifyPeerHeads(ctx, st, hub, workspaceOf(t, st), self.ID)
	if err != nil {
		t.Fatalf("verify after backfill: %v", err)
	}
	if n != 0 {
		t.Fatalf("a backfilled stream must not re-alarm, got %d", n)
	}
	if got := len(openOmissionConflicts(t, st)); got != 0 {
		t.Fatalf("omission conflict must resolve once the peer fold catches up, still open: %d", got)
	}
}

// TestVerifyPeerHeadsNoFalsePositiveOnCompleteStream: when B holds the whole
// stream the head commits to, no cycle raises an alarm.
func TestVerifyPeerHeadsNoFalsePositiveOnCompleteStream(t *testing.T) {
	ctx := context.Background()
	st, self, events, signing := peerHeadFixture(t, 10, 10) // B has everything
	hub := testFileHub(t)
	putPeerHead(t, st, hub, signing, 10, peerFoldAt(t, st, events, 10))

	for cycle := 0; cycle < 3; cycle++ {
		n, err := VerifyPeerHeads(ctx, st, hub, workspaceOf(t, st), self.ID)
		if err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
		if n != 0 {
			t.Fatalf("cycle %d falsely alarmed", cycle)
		}
	}
	if got := len(openOmissionConflicts(t, st)); got != 0 {
		t.Fatalf("complete stream must not create omission conflicts, got %d", got)
	}
}

// TestVerifyPeerHeadsDetectsFork: B holds the full stream, but the peer's signed
// head commits to a DIFFERENT fold at the same seq (equivocation / a spliced
// stream). B's independent fold disagrees, so it raises a fork immediately.
func TestVerifyPeerHeadsDetectsFork(t *testing.T) {
	ctx := context.Background()
	st, self, events, signing := peerHeadFixture(t, 10, 10)
	hub := testFileHub(t)

	// A validly-signed head over a fold that does NOT match the real stream.
	bogus := fold.Encode(fold.Step(fold.Seed(workspaceOf(t, st), "device-peer"), 10, "sha256:forged"))
	putPeerHead(t, st, hub, signing, 10, bogus)

	n, err := VerifyPeerHeads(ctx, st, hub, workspaceOf(t, st), self.ID)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 fork detection, got %d", n)
	}
	conflicts := openOmissionConflicts(t, st)
	if len(conflicts) != 1 {
		t.Fatalf("want 1 fork conflict, got %d", len(conflicts))
	}
	_, kind, _, err := ParseOmissionConflictDetails(conflicts[0].DetailsJSON)
	if err != nil {
		t.Fatal(err)
	}
	if kind != OmissionKindFork {
		t.Fatalf("kind = %s, want fork", kind)
	}
	_ = events
}

// TestVerifyPeerHeadsIgnoresUnapprovedPeer: a head from a non-approved device
// carries no authority and is never checked (fail-safe).
func TestVerifyPeerHeadsIgnoresUnapprovedPeer(t *testing.T) {
	ctx := context.Background()
	st, self := newSyncStore(t)
	signing := addRemoteDeviceForApplyTest(t, st, "device-peer", "pending") // NOT approved
	hub := testFileHub(t)
	// A head claiming seq 5 with any fold; B holds nothing.
	putPeerHead(t, st, hub, signing, 5, fold.Encode(fold.Seed(workspaceOf(t, st), "device-peer")))

	for cycle := 0; cycle < 2; cycle++ {
		n, err := VerifyPeerHeads(ctx, st, hub, workspaceOf(t, st), self.ID)
		if err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
		if n != 0 {
			t.Fatalf("cycle %d: an unapproved peer's head must be ignored", cycle)
		}
	}
}

func workspaceOf(t *testing.T, st *state.Store) string {
	t.Helper()
	ws, err := st.WorkspaceID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return ws
}
