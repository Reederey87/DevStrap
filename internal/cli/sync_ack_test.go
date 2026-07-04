package cli

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
)

// ackCountingHub wraps a Hub to count PutAck calls and optionally fail them.
type ackCountingHub struct {
	dssync.Hub
	puts   int
	putErr error
}

func (h *ackCountingHub) PutAck(ctx context.Context, deviceID string, raw []byte) error {
	h.puts++
	if h.putErr != nil {
		return h.putErr
	}
	return h.Hub.PutAck(ctx, deviceID, raw)
}

// readLocalAck fetches and parses the local device's ack from the file hub.
func readLocalAck(t *testing.T, env *recoveryEnv, store *state.Store) (dssync.AckMarker, bool) {
	t.Helper()
	dev, err := store.CurrentDevice(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	acks, err := (dssync.FileHub{Path: env.hubPath}).ListAcks(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw, ok := acks[dev.ID]
	if !ok {
		return dssync.AckMarker{}, false
	}
	m, err := dssync.ParseAckMarker(raw)
	if err != nil {
		t.Fatal(err)
	}
	return m, true
}

// TestSyncAckWrittenAfterCleanCycle asserts a clean cycle publishes a verifiable
// ack whose cursor, push watermark, and HLC watermark match the store.
func TestSyncAckWrittenAfterCleanCycle(t *testing.T) {
	env, store, _ := setupRecovery(t, true)
	defer closeStore(store)
	hub := env.hub(t, store)
	if err := store.AdvanceHubDeviceCursor(env.ctx, env.hubID, recoveryProducer, 7); err != nil {
		t.Fatal(err)
	}
	if err := store.AdvancePushSeqCursor(env.ctx, env.hubID, 3); err != nil {
		t.Fatal(err)
	}
	wantHLC, err := store.CurrentHLC(env.ctx)
	if err != nil {
		t.Fatal(err)
	}

	maybeWriteSyncAck(env.ctx, store, hub, env.hubID, env.paths, pullApplyOutcome{}, false)

	m, ok := readLocalAck(t, env, store)
	if !ok {
		t.Fatal("no ack written after a clean cycle")
	}
	dev, _ := store.CurrentDevice(env.ctx)
	if err := dssync.VerifyAckMarker(m, dev.SigningPublicKey); err != nil {
		t.Fatalf("ack failed verification: %v", err)
	}
	if m.Cursor[recoveryProducer] != 7 {
		t.Errorf("ack cursor[producer] = %d, want 7", m.Cursor[recoveryProducer])
	}
	if m.PushedThroughSeq != 3 {
		t.Errorf("ack pushed_through_seq = %d, want 3", m.PushedThroughSeq)
	}
	if m.HLCWatermark != wantHLC || m.ProducedAt != wantHLC {
		t.Errorf("ack watermark/producedAt = %d/%d, want %d", m.HLCWatermark, m.ProducedAt, wantHLC)
	}
	if m.WorkspaceID != env.wsID {
		t.Errorf("ack workspace = %q, want %q", m.WorkspaceID, env.wsID)
	}
}

// TestSyncAckSuppressedByDirtyConditions: each incomplete-view signal suppresses
// the ack.
func TestSyncAckSuppressedByDirtyConditions(t *testing.T) {
	cases := []struct {
		name     string
		pull     pullApplyOutcome
		deferred bool
		skip     bool // insert an open skipped-event row
	}{
		{name: "deferred push", deferred: true},
		{name: "quarantined apply", pull: pullApplyOutcome{stats: dssync.ApplyStats{Quarantined: 1}}},
		{name: "cursor held", pull: pullApplyOutcome{stats: dssync.ApplyStats{CursorHeld: true}}},
		{name: "open skipped event", skip: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, store, _ := setupRecovery(t, true)
			defer closeStore(store)
			hub := env.hub(t, store)
			if tc.skip {
				if _, err := store.NoteSkippedEvent(env.ctx, state.Event{ID: "evt_skip", DeviceID: recoveryProducer, Seq: 1, HLC: 5}, "unsupported"); err != nil {
					t.Fatal(err)
				}
			}
			maybeWriteSyncAck(env.ctx, store, hub, env.hubID, env.paths, tc.pull, tc.deferred)
			if _, ok := readLocalAck(t, env, store); ok {
				t.Fatalf("%s: ack written despite an incomplete view", tc.name)
			}
		})
	}
}

// TestSyncAckSkipsUnchangedCycle: a second clean cycle with the same cursor and
// push watermark does not re-PUT the ack.
func TestSyncAckSkipsUnchangedCycle(t *testing.T) {
	env, store, _ := setupRecovery(t, true)
	defer closeStore(store)
	hub := &ackCountingHub{Hub: env.hub(t, store)}
	if err := store.AdvancePushSeqCursor(env.ctx, env.hubID, 2); err != nil {
		t.Fatal(err)
	}
	maybeWriteSyncAck(env.ctx, store, hub, env.hubID, env.paths, pullApplyOutcome{}, false)
	maybeWriteSyncAck(env.ctx, store, hub, env.hubID, env.paths, pullApplyOutcome{}, false)
	if hub.puts != 1 {
		t.Fatalf("PutAck called %d times across two unchanged cycles, want 1", hub.puts)
	}
	// A change in the push watermark forces a fresh ack.
	if err := store.AdvancePushSeqCursor(env.ctx, env.hubID, 5); err != nil {
		t.Fatal(err)
	}
	maybeWriteSyncAck(env.ctx, store, hub, env.hubID, env.paths, pullApplyOutcome{}, false)
	if hub.puts != 2 {
		t.Fatalf("PutAck called %d times after a watermark change, want 2", hub.puts)
	}
}

// TestSyncAckPutFailureDoesNotWedge: a PutAck failure is swallowed and does NOT
// cache the marker, so the next clean cycle retries.
func TestSyncAckPutFailureDoesNotWedge(t *testing.T) {
	env, store, _ := setupRecovery(t, true)
	defer closeStore(store)
	failing := &ackCountingHub{Hub: env.hub(t, store), putErr: errors.New("upload failed")}
	maybeWriteSyncAck(env.ctx, store, failing, env.hubID, env.paths, pullApplyOutcome{}, false)
	if failing.puts != 1 {
		t.Fatalf("PutAck calls = %d, want 1", failing.puts)
	}
	if _, ok, err := store.GetLocalMeta(env.ctx, syncAckCacheKey(env.hubID)); err != nil || ok {
		t.Fatalf("failed PutAck must not cache the marker: ok=%v err=%v", ok, err)
	}
	// A retry against a working hub now succeeds and writes the ack.
	maybeWriteSyncAck(env.ctx, store, env.hub(t, store), env.hubID, env.paths, pullApplyOutcome{}, false)
	if _, ok := readLocalAck(t, env, store); !ok {
		t.Fatal("retry after PutAck failure did not write the ack")
	}
}

// TestCleanupRevokedStreamsReclaimsStreamAndAck: a revoked device whose stream
// the floors fully cover has its event objects and ack removed; an approved
// device and a floor-less revoked device are untouched.
func TestCleanupRevokedStreamsReclaimsStreamAndAck(t *testing.T) {
	env, store, _ := setupRecovery(t, true)
	defer closeStore(store)
	fh := dssync.FileHub{Path: env.hubPath}
	// Two devices with events on the hub.
	revoked := "dev_revoked"
	if err := store.UpsertDevice(env.ctx, state.Device{
		ID: revoked, Name: "rev", OS: "linux", Arch: "arm64",
		PublicKey: "age1x", SigningPublicKey: "sig", TrustState: "revoked",
	}); err != nil {
		t.Fatal(err)
	}
	if err := fh.Push(env.ctx, []state.Event{
		{ID: "r1", DeviceID: revoked, HLC: 1, Seq: 1},
		{ID: "r2", DeviceID: revoked, HLC: 2, Seq: 2},
		{ID: "p1", DeviceID: recoveryProducer, HLC: 3, Seq: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := fh.PutAck(env.ctx, revoked, []byte(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}
	// Floors cover the revoked device's whole stream (floor > max seq).
	floors := map[string]int64{revoked: 3, recoveryProducer: 2}
	reclaimed, err := cleanupRevokedStreams(env.ctx, io.Discard, store, env.hub(t, store), floors)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed != 2 {
		t.Fatalf("reclaimed = %d, want 2", reclaimed)
	}
	remaining, err := fh.Pull(env.ctx, dssync.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].DeviceID != recoveryProducer {
		t.Fatalf("revoked stream not reclaimed; remaining = %+v", remaining)
	}
	acks, err := fh.ListAcks(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := acks[revoked]; ok {
		t.Error("revoked device ack was not deleted")
	}
}

// TestCleanupRevokedStreamsSkipsFloorlessDevice: a revoked device the compactor
// never consumed (absent from floors) is left alone.
func TestCleanupRevokedStreamsSkipsFloorlessDevice(t *testing.T) {
	env, store, _ := setupRecovery(t, true)
	defer closeStore(store)
	fh := dssync.FileHub{Path: env.hubPath}
	revoked := "dev_revoked"
	if err := store.UpsertDevice(env.ctx, state.Device{
		ID: revoked, Name: "rev", OS: "linux", Arch: "arm64",
		PublicKey: "age1x", SigningPublicKey: "sig", TrustState: "revoked",
	}); err != nil {
		t.Fatal(err)
	}
	if err := fh.Push(env.ctx, []state.Event{{ID: "r1", DeviceID: revoked, HLC: 1, Seq: 1}}); err != nil {
		t.Fatal(err)
	}
	// Floors do NOT include the revoked device.
	reclaimed, err := cleanupRevokedStreams(env.ctx, io.Discard, store, env.hub(t, store), map[string]int64{recoveryProducer: 2})
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed != 0 {
		t.Fatalf("reclaimed = %d, want 0 (floorless revoked device must be skipped)", reclaimed)
	}
	remaining, err := fh.Pull(env.ctx, dssync.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 {
		t.Fatalf("floorless revoked stream was wrongly reclaimed; remaining = %+v", remaining)
	}
}
