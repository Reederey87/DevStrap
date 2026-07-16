package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
)

// setupCompact builds a founder device (local) that holds the epoch-1 WCK and
// has two local projects to snapshot, reusing the recovery scaffolding (which
// also pins an approved peer, recoveryProducer, whose signing key is known for
// competing-manifest scenarios). Returns the env, store, and the local device id.
func setupCompact(t *testing.T) (*recoveryEnv, *state.Store, string) {
	t.Helper()
	env, store, _ := setupRecovery(t, true)
	for _, p := range []struct{ remote, path string }{
		{"git@github.com:acme/api.git", "work/api"},
		{"git@github.com:acme/web.git", "work/web"},
	} {
		if _, err := addProject(env.ctx, store, env.opts, p.remote, p.path, "", ""); err != nil {
			t.Fatalf("addProject %s: %v", p.path, err)
		}
	}
	dev, err := store.CurrentDevice(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	return env, store, dev.ID
}

func readHubEvents(t *testing.T, hubPath string) []state.Event {
	t.Helper()
	raw, err := os.ReadFile(hubPath)
	if err != nil {
		t.Fatalf("read hub file: %v", err)
	}
	if len(raw) == 0 {
		return nil
	}
	var events []state.Event
	if err := json.Unmarshal(raw, &events); err != nil {
		t.Fatalf("decode hub file: %v", err)
	}
	return events
}

// signedRetentionManifest builds and signs a retention manifest for injecting a
// competing / pre-existing head object into the FileHub.
func signedRetentionManifest(t *testing.T, wsID string, floors map[string]int64, producedBy, signPriv string) []byte {
	t.Helper()
	m := dssync.RetentionManifest{
		WorkspaceID: wsID,
		Floors:      floors,
		Snapshot: dssync.RetentionSnapshotRef{
			Epoch: 1, HLC: 1 << 16, KID: "kid-placeholder", ProducedBy: producedBy,
			SHA256: strings.Repeat("a", 64),
		},
		ProducedBy: producedBy,
		ProducedAt: 1 << 16,
	}
	if err := dssync.SignRetentionManifest(&m, signPriv); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestHubCompactHappyPath: a founder compacts, publishing a signed snapshot +
// manifest and deleting the now-cold events below the floor.
func TestHubCompactHappyPath(t *testing.T) {
	env, store, selfID := setupCompact(t)
	defer closeStore(store)

	var out bytes.Buffer
	if err := hubCompact(env.ctx, &out, io.Discard, env.opts, store, env.hub(t, store), env.hubID, env.paths, 2, 0, true, false); err != nil {
		t.Fatalf("hubCompact: %v", err)
	}
	fh := dssync.FileHub{Path: env.hubPath}
	raw, _, err := fh.GetRetention(env.ctx)
	if err != nil {
		t.Fatalf("GetRetention: %v", err)
	}
	m, err := dssync.ParseRetentionManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if m.ProducedBy != selfID {
		t.Errorf("manifest producer = %s, want %s", m.ProducedBy, selfID)
	}
	if m.Floors[selfID] != 3 {
		t.Errorf("floor[self] = %d, want 3 (two events pushed)", m.Floors[selfID])
	}
	if _, err := fh.GetSnapshotObject(env.ctx, m.Snapshot.SHA256); err != nil {
		t.Fatalf("snapshot object missing: %v", err)
	}
	if remaining := readHubEvents(t, env.hubPath); len(remaining) != 0 {
		t.Fatalf("cold events not deleted: %d remain", len(remaining))
	}
	if !bytes.Contains(out.Bytes(), []byte("published snapshot")) {
		t.Errorf("summary = %q, want a published-snapshot summary", out.String())
	}
	// The compactor's own pull cursor was advanced to floor-1, so a re-sync is
	// incremental (no self-snapshot demand) — a second compact must succeed.
	if err := hubCompact(env.ctx, io.Discard, io.Discard, env.opts, store, env.hub(t, store), env.hubID, env.paths, 2, 0, true, false); err != nil {
		t.Fatalf("second hubCompact: %v", err)
	}
}

// TestHubCompactDryRunWritesNothing: a dry run prints the plan but writes no
// snapshot, no manifest, and deletes no events.
func TestHubCompactDryRunWritesNothing(t *testing.T) {
	env, store, _ := setupCompact(t)
	defer closeStore(store)

	var out bytes.Buffer
	if err := hubCompact(env.ctx, &out, io.Discard, env.opts, store, env.hub(t, store), env.hubID, env.paths, 2, 0, true, true); err != nil {
		t.Fatalf("hubCompact --dry-run: %v", err)
	}
	fh := dssync.FileHub{Path: env.hubPath}
	if _, _, err := fh.GetRetention(env.ctx); !errors.Is(err, dssync.ErrRetentionNotFound) {
		t.Fatalf("dry run wrote a retention manifest: %v", err)
	}
	objs, err := fh.ListSnapshotObjects(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 0 {
		t.Fatalf("dry run wrote %d snapshot object(s), want 0", len(objs))
	}
	if !bytes.Contains(out.Bytes(), []byte("dry run")) {
		t.Errorf("dry-run output = %q, want a dry-run plan", out.String())
	}
}

// TestHubCompactMinEventsRefusal: --min-events above the deletable count refuses
// before any hub write.
func TestHubCompactMinEventsRefusal(t *testing.T) {
	env, store, _ := setupCompact(t)
	defer closeStore(store)

	// Two events would be deleted (floor 3 → seqs 1,2); require 5.
	err := hubCompact(env.ctx, io.Discard, io.Discard, env.opts, store, env.hub(t, store), env.hubID, env.paths, 2, 5, true, false)
	if err == nil || !strings.Contains(err.Error(), "min-events") {
		t.Fatalf("hubCompact err = %v, want a --min-events refusal", err)
	}
	fh := dssync.FileHub{Path: env.hubPath}
	if _, _, gerr := fh.GetRetention(env.ctx); !errors.Is(gerr, dssync.ErrRetentionNotFound) {
		t.Fatalf("min-events refusal still wrote a manifest: %v", gerr)
	}
}

// TestHubCompactGateRefusesOnOpenConflict: an open quarantine-class conflict
// makes compact refuse (shared refuseIfIncompleteView gate), writing nothing.
func TestHubCompactGateRefusesOnOpenConflict(t *testing.T) {
	env, store, _ := setupCompact(t)
	defer closeStore(store)
	if err := store.InsertConflict(env.ctx, "", dssync.ConflictEventVerification, `{"event_id":"evt_q"}`); err != nil {
		t.Fatalf("InsertConflict: %v", err)
	}
	err := hubCompact(env.ctx, io.Discard, io.Discard, env.opts, store, env.hub(t, store), env.hubID, env.paths, 2, 0, true, false)
	if !errors.Is(err, errGCRefused) {
		t.Fatalf("hubCompact err = %v, want errGCRefused (shared incomplete-view gate)", err)
	}
	fh := dssync.FileHub{Path: env.hubPath}
	if _, _, gerr := fh.GetRetention(env.ctx); !errors.Is(gerr, dssync.ErrRetentionNotFound) {
		t.Fatalf("refusal still wrote a manifest: %v", gerr)
	}
}

// TestHubCompactProceedsWithOpenOmissionConflict is the H1(a) recovery property
// at the gate level: an open `event_omission` conflict — the alarm P4-SYNC-05
// raises for a permanent per-device stream gap — must NOT block `hub compact`,
// because compact is the documented cure for exactly that gap (publish a floor
// above the stranded slot; the affected device re-bootstraps past it). The
// detection guarantee is preserved for `hub gc` (which deletes blobs), which
// still refuses on the same conflict.
func TestHubCompactProceedsWithOpenOmissionConflict(t *testing.T) {
	env, store, _ := setupCompact(t)
	defer closeStore(store)

	// The only open conflict is an omission alarm for a withheld/backfilled gap.
	if err := store.InsertConflict(env.ctx, "", dssync.ConflictEventOmission,
		`{"device_id":"dev_peer","kind":"withheld_tail","local_seq":2}`); err != nil {
		t.Fatalf("InsertConflict: %v", err)
	}

	// compact must COMPLETE (the cure is reachable), writing a manifest.
	if err := hubCompact(env.ctx, io.Discard, io.Discard, env.opts, store, env.hub(t, store), env.hubID, env.paths, 2, 0, true, false); err != nil {
		t.Fatalf("hubCompact must proceed despite an open omission conflict, got: %v", err)
	}
	fh := dssync.FileHub{Path: env.hubPath}
	if _, _, err := fh.GetRetention(env.ctx); err != nil {
		t.Fatalf("compact should have published a manifest: %v", err)
	}

	// The SAME conflict must still refuse `hub gc` (detection preserved: gc
	// deletes blobs on an incomplete view, so its omission gate stays closed).
	if _, _, _, gerr := hubGC(env.ctx, io.Discard, store, env.hub(t, store), env.hubID, env.paths, 2, 0, false); !errors.Is(gerr, errGCRefused) {
		t.Fatalf("hubGC err = %v, want errGCRefused on the open omission conflict", gerr)
	}
}

// TestHubCompactGateRefusesOnKeyGrantWait: the NEW gate — an open key_grant_waits
// row means this device cannot decrypt part of the log, so compact refuses.
func TestHubCompactGateRefusesOnKeyGrantWait(t *testing.T) {
	env, store, _ := setupCompact(t)
	defer closeStore(store)
	if _, err := store.NoteMissingKeyGrant(env.ctx, 7, strings.Repeat("b", 64)); err != nil {
		t.Fatalf("NoteMissingKeyGrant: %v", err)
	}
	err := hubCompact(env.ctx, io.Discard, io.Discard, env.opts, store, env.hub(t, store), env.hubID, env.paths, 2, 0, true, false)
	if !errors.Is(err, errGCRefused) {
		t.Fatalf("hubCompact err = %v, want errGCRefused on an open key grant wait", err)
	}
	if !strings.Contains(err.Error(), "key grant") {
		t.Errorf("refusal = %v, want it to name the awaited key grant", err)
	}
}

// TestHubCompactKeylessJoinerRefuses: a keyless joiner cannot compact — its push
// defers awaiting a grant, so compact refuses without writing a manifest.
func TestHubCompactKeylessJoinerRefuses(t *testing.T) {
	env, store, _ := setupRecovery(t, false) // B holds no WCK
	defer closeStore(store)
	env.opts.v.Set("role", "joiner") // never self-founds a key
	if _, err := addProject(env.ctx, store, env.opts, "git@github.com:acme/api.git", "work/api", "", ""); err != nil {
		t.Fatalf("addProject: %v", err)
	}
	err := hubCompact(env.ctx, io.Discard, io.Discard, env.opts, store, env.hub(t, store), env.hubID, env.paths, 2, 0, true, false)
	if err == nil || !strings.Contains(err.Error(), "cannot compact") {
		t.Fatalf("hubCompact err = %v, want a keyless cannot-compact refusal", err)
	}
	fh := dssync.FileHub{Path: env.hubPath}
	if _, _, gerr := fh.GetRetention(env.ctx); !errors.Is(gerr, dssync.ErrRetentionNotFound) {
		t.Fatalf("keyless refusal still wrote a manifest: %v", gerr)
	}
}

// TestReconcileCompactFloorsMonotonicity: reconciling new floors against a
// current manifest with a HIGHER floor for a device is refused (floors are
// monotonic).
func TestReconcileCompactFloorsMonotonicity(t *testing.T) {
	env, store, selfID := setupCompact(t)
	defer closeStore(store)
	self, err := store.CurrentDevice(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Current manifest signed by the approved producer sets floor[self]=9.
	current := signedRetentionManifest(t, env.wsID, map[string]int64{selfID: 9}, recoveryProducer, env.prodSign.Private)
	_, _, rerr := reconcileCompactFloors(env.ctx, store, self, dssync.Cursor{selfID: 3}, current)
	if !errors.Is(rerr, dssync.ErrRetentionRollback) {
		t.Fatalf("reconcile err = %v, want ErrRetentionRollback", rerr)
	}
}

// TestReconcileCompactFloorsCarriesForwardAbsentDevice: a device present in the
// current manifest but absent from the new floors keeps its old floor.
func TestReconcileCompactFloorsCarriesForwardAbsentDevice(t *testing.T) {
	env, store, selfID := setupCompact(t)
	defer closeStore(store)
	self, err := store.CurrentDevice(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	current := signedRetentionManifest(t, env.wsID, map[string]int64{recoveryProducer: 7}, recoveryProducer, env.prodSign.Private)
	floors, prev, rerr := reconcileCompactFloors(env.ctx, store, self, dssync.Cursor{selfID: 3}, current)
	if rerr != nil {
		t.Fatalf("reconcile: %v", rerr)
	}
	if floors[recoveryProducer] != 7 {
		t.Errorf("carried floor[producer] = %d, want 7", floors[recoveryProducer])
	}
	if floors[selfID] != 3 {
		t.Errorf("floor[self] = %d, want 3", floors[selfID])
	}
	if prev == "" {
		t.Error("prev sha256 should be set when building on an existing manifest")
	}
}

// TestReconcileCompactFloorsRefusesUnapprovedProducer: building on a manifest
// whose producer is not a locally approved (or the local) device is refused.
func TestReconcileCompactFloorsRefusesUnapprovedProducer(t *testing.T) {
	env, store, selfID := setupCompact(t)
	defer closeStore(store)
	self, err := store.CurrentDevice(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetDeviceTrustState(env.ctx, recoveryProducer, "pending"); err != nil {
		t.Fatal(err)
	}
	current := signedRetentionManifest(t, env.wsID, map[string]int64{selfID: 2}, recoveryProducer, env.prodSign.Private)
	_, _, rerr := reconcileCompactFloors(env.ctx, store, self, dssync.Cursor{selfID: 3}, current)
	if !errors.Is(rerr, dssync.ErrSnapshotVerification) {
		t.Fatalf("reconcile err = %v, want ErrSnapshotVerification", rerr)
	}
}

// recordingHub wraps a Hub to record the order of snapshot/retention/compact
// writes and optionally inject a one-shot side effect before the first
// PutRetention (used to force a CAS conflict). The EncryptedHub type assertion
// in the gate does not see through this wrapper, which is fine for the converged
// single-device scenarios these tests build.
type recordingHub struct {
	dssync.Hub
	mu                 sync.Mutex
	calls              []string
	beforePutRetention func()
	fired              bool
}

func (h *recordingHub) record(s string) {
	h.mu.Lock()
	h.calls = append(h.calls, s)
	h.mu.Unlock()
}

func (h *recordingHub) PutSnapshotObject(ctx context.Context, sha string, body []byte) error {
	h.record("put-snapshot")
	return h.Hub.PutSnapshotObject(ctx, sha, body)
}

func (h *recordingHub) PutRetention(ctx context.Context, raw []byte, ifMatchETag string) error {
	if h.beforePutRetention != nil && !h.fired {
		h.fired = true
		h.beforePutRetention()
	}
	h.record("put-retention")
	return h.Hub.PutRetention(ctx, raw, ifMatchETag)
}

func (h *recordingHub) CompactEventsBelow(ctx context.Context, floors dssync.Cursor) (int, error) {
	h.record("compact")
	return h.Hub.CompactEventsBelow(ctx, floors)
}

func firstIndex(calls []string, want string) int {
	for i, c := range calls {
		if c == want {
			return i
		}
	}
	return -1
}

// TestHubCompactConfirmsBeforeDelete: PutSnapshotObject and PutRetention both
// precede any CompactEventsBelow — the confirm-before-delete invariant.
func TestHubCompactConfirmsBeforeDelete(t *testing.T) {
	env, store, _ := setupCompact(t)
	defer closeStore(store)
	rec := &recordingHub{Hub: env.hub(t, store)}
	if err := hubCompact(env.ctx, io.Discard, io.Discard, env.opts, store, rec, env.hubID, env.paths, 2, 0, true, false); err != nil {
		t.Fatalf("hubCompact: %v", err)
	}
	del := firstIndex(rec.calls, "compact")
	if del < 0 {
		t.Fatalf("no CompactEventsBelow call recorded: %v", rec.calls)
	}
	snap := firstIndex(rec.calls, "put-snapshot")
	ret := firstIndex(rec.calls, "put-retention")
	if snap < 0 || snap > del {
		t.Errorf("PutSnapshotObject (%d) must precede CompactEventsBelow (%d): %v", snap, del, rec.calls)
	}
	if ret < 0 || ret > del {
		t.Errorf("PutRetention (%d) must precede CompactEventsBelow (%d): %v", ret, del, rec.calls)
	}
}

// TestHubCompactCASConflictRetriesOnce: a competing manifest written between the
// compactor's read and its create-only PutRetention forces one ErrRetentionConflict;
// compact re-reads, re-reconciles, and retries the CAS successfully.
func TestHubCompactCASConflictRetriesOnce(t *testing.T) {
	env, store, selfID := setupCompact(t)
	defer closeStore(store)
	fh := dssync.FileHub{Path: env.hubPath}
	rec := &recordingHub{Hub: env.hub(t, store)}
	rec.beforePutRetention = func() {
		// A valid competing manifest signed by the approved producer, with a
		// harmless floor that keeps our floors monotonic on retry.
		competing := signedRetentionManifest(t, env.wsID, map[string]int64{recoveryProducer: 1}, recoveryProducer, env.prodSign.Private)
		if err := fh.PutRetention(env.ctx, competing, ""); err != nil {
			t.Errorf("inject competing manifest: %v", err)
		}
	}
	if err := hubCompact(env.ctx, io.Discard, io.Discard, env.opts, store, rec, env.hubID, env.paths, 2, 0, true, false); err != nil {
		t.Fatalf("hubCompact with one CAS conflict: %v", err)
	}
	// The head is now OUR manifest (producer self), and the retry re-put once.
	raw, _, err := fh.GetRetention(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	m, err := dssync.ParseRetentionManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if m.ProducedBy != selfID {
		t.Errorf("head manifest producer = %s, want self %s after retry", m.ProducedBy, selfID)
	}
	retentionPuts := 0
	for _, c := range rec.calls {
		if c == "put-retention" {
			retentionPuts++
		}
	}
	if retentionPuts != 2 {
		t.Errorf("PutRetention called %d times, want 2 (one conflict + one retry)", retentionPuts)
	}
}

// TestHubCompactPrunesOldSnapshots: with --keep-snapshots 1, a second compaction
// prunes the superseded snapshot, leaving only the newly-referenced one.
func TestHubCompactPrunesOldSnapshots(t *testing.T) {
	env, store, _ := setupCompact(t)
	defer closeStore(store)
	if err := hubCompact(env.ctx, io.Discard, io.Discard, env.opts, store, env.hub(t, store), env.hubID, env.paths, 2, 0, true, false); err != nil {
		t.Fatalf("first hubCompact: %v", err)
	}
	// A new local project gives the second compaction something to advance.
	if _, err := addProject(env.ctx, store, env.opts, "git@github.com:acme/cli.git", "work/cli", "", ""); err != nil {
		t.Fatalf("addProject: %v", err)
	}
	if err := hubCompact(env.ctx, io.Discard, io.Discard, env.opts, store, env.hub(t, store), env.hubID, env.paths, 1, 0, true, false); err != nil {
		t.Fatalf("second hubCompact: %v", err)
	}
	fh := dssync.FileHub{Path: env.hubPath}
	objs, err := fh.ListSnapshotObjects(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 {
		t.Fatalf("snapshot objects = %d, want 1 (keep-snapshots=1 pruned the old one)", len(objs))
	}
	raw, _, err := fh.GetRetention(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	m, err := dssync.ParseRetentionManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if objs[0].Key != m.Snapshot.SHA256 {
		t.Errorf("retained snapshot %s is not the manifest-referenced one %s", objs[0].Key, m.Snapshot.SHA256)
	}
}
