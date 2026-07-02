package sync

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/id"
	"github.com/Reederey87/DevStrap/internal/state"
)

func newSyncStore(t *testing.T) (*state.Store, state.Device) {
	t.Helper()
	st, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(context.Background(), "test", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	device, err := st.EnsureDevice(context.Background(), "device-a")
	if err != nil {
		t.Fatal(err)
	}
	return st, device
}

func renameEvent(t *testing.T, deviceID string, hlc int64, oldPath, newPath string) state.Event {
	t.Helper()
	raw, err := json.Marshal(RenamePayload{OldPath: oldPath, NewPath: newPath})
	if err != nil {
		t.Fatal(err)
	}
	eid, err := id.New("evt")
	if err != nil {
		t.Fatal(err)
	}
	return state.Event{
		ID:          eid,
		DeviceID:    deviceID,
		HLC:         hlc,
		Type:        EventProjectRenamed,
		PayloadJSON: string(raw),
		ContentHash: state.ContentHash(string(raw)),
	}
}

// SYNC-3: a remote event whose physical timestamp is far beyond the trusted
// skew is quarantined, not applied, and does not abort the batch.
func TestApplyEventsQuarantinesFarFutureRemoteEvent(t *testing.T) {
	ctx := context.Background()
	st, device := newSyncStore(t)

	futurePhysical := time.Now().Add(time.Hour).UnixMilli()
	poison, err := NewProjectEvent(device.ID, EventProjectAdded, futurePhysical<<hlcLogicalBits, ProjectPayload{
		Path: "work/acme/poison", Type: "git_repo", RemoteKey: "github.com/acme/poison",
	})
	if err != nil {
		t.Fatal(err)
	}
	good, err := NewProjectEvent(device.ID, EventProjectAdded, time.Now().UnixMilli()<<hlcLogicalBits, ProjectPayload{
		Path: "work/acme/good", Type: "git_repo", RemoteKey: "github.com/acme/good",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEvents(ctx, st, []state.Event{poison, good}); err != nil {
		t.Fatalf("ApplyEvents should not abort on a quarantined event: %v", err)
	}
	projects, err := st.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].Path != "work/acme/good" {
		t.Fatalf("projects = %+v, want only the good project applied", projects)
	}
	conflicts, err := st.OpenConflicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !hasConflictType(conflicts, "untrustworthy_remote_time") {
		t.Fatalf("conflicts = %+v, want untrustworthy_remote_time", conflicts)
	}
}

// SYNC-3: applying a remote event advances the local clock so the next local
// event sorts causally after it.
func TestApplyEventsAdvancesLocalClockOnReceive(t *testing.T) {
	ctx := context.Background()
	st, device := newSyncStore(t)

	remoteHLC := time.Now().UnixMilli() << hlcLogicalBits
	remote, err := NewProjectEvent(device.ID, EventProjectAdded, remoteHLC, ProjectPayload{
		Path: "work/acme/api", Type: "git_repo", RemoteKey: "github.com/acme/api",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEvents(ctx, st, []state.Event{remote}); err != nil {
		t.Fatal(err)
	}
	local, err := CreateProjectEvent(ctx, st, EventProjectAdded, ProjectPayload{
		Path: "work/acme/other", Type: "git_repo", RemoteKey: "github.com/acme/other",
	})
	if err != nil {
		t.Fatal(err)
	}
	if local.HLC <= remoteHLC {
		t.Fatalf("local HLC %d did not advance past received remote HLC %d", local.HLC, remoteHLC)
	}
}

// SYNC-5: a remote rename moves the entry; renaming onto an existing active
// entry records a conflict instead of overwriting.
func TestApplyEventsRenameMovesProjectAndConflictsOnCollision(t *testing.T) {
	ctx := context.Background()
	st, device := newSyncStore(t)

	add, err := NewProjectEvent(device.ID, EventProjectAdded, 10<<hlcLogicalBits, ProjectPayload{
		Path: "work/acme/old", Type: "git_repo", RemoteKey: "github.com/acme/api",
	})
	if err != nil {
		t.Fatal(err)
	}
	ren := renameEvent(t, device.ID, 20<<hlcLogicalBits, "work/acme/old", "work/acme/new")
	if _, err := ApplyEvents(ctx, st, []state.Event{add, ren}); err != nil {
		t.Fatal(err)
	}
	projects, err := st.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].Path != "work/acme/new" {
		t.Fatalf("projects after rename = %+v, want work/acme/new", projects)
	}

	addOther, err := NewProjectEvent(device.ID, EventProjectAdded, 30<<hlcLogicalBits, ProjectPayload{
		Path: "work/acme/other", Type: "git_repo", RemoteKey: "github.com/acme/other",
	})
	if err != nil {
		t.Fatal(err)
	}
	collide := renameEvent(t, device.ID, 40<<hlcLogicalBits, "work/acme/other", "work/acme/new")
	if _, err := ApplyEvents(ctx, st, []state.Event{addOther, collide}); err != nil {
		t.Fatal(err)
	}
	projects, err = st.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 2 {
		t.Fatalf("projects after collision rename = %+v, want both kept", projects)
	}
	conflicts, err := st.OpenConflicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !hasConflictType(conflicts, "rename_target_exists") {
		t.Fatalf("conflicts = %+v, want rename_target_exists", conflicts)
	}
}

// SYNC-5: a remote delete must not destroy a dirty local checkout; it raises a
// pending_delete_conflict and leaves the project active.
func TestApplyEventsDeleteVsDirtyRaisesConflict(t *testing.T) {
	ctx := context.Background()
	st, device := newSyncStore(t)

	add, err := NewProjectEvent(device.ID, EventProjectAdded, 10<<hlcLogicalBits, ProjectPayload{
		Path: "work/acme/api", Type: "git_repo", RemoteKey: "github.com/acme/api",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEvents(ctx, st, []state.Event{add}); err != nil {
		t.Fatal(err)
	}
	project, err := st.ProjectByPath(ctx, "work/acme/api")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateProjectLocalState(ctx, project.ID, "/tmp/Code/work/acme/api", "available", "dirty"); err != nil {
		t.Fatal(err)
	}
	del, err := NewProjectEvent(device.ID, EventProjectDeleted, 20<<hlcLogicalBits, ProjectPayload{Path: "work/acme/api"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEvents(ctx, st, []state.Event{del}); err != nil {
		t.Fatal(err)
	}
	projects, err := st.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 {
		t.Fatalf("dirty project must survive a remote delete: %+v", projects)
	}
	conflicts, err := st.OpenConflicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !hasConflictType(conflicts, "pending_delete_conflict") {
		t.Fatalf("conflicts = %+v, want pending_delete_conflict", conflicts)
	}
}

// SYNC-5: tombstone GC purges deleted entries below the supplied HLC only.
func TestGCTombstonesPurgesBelowHLC(t *testing.T) {
	ctx := context.Background()
	st, device := newSyncStore(t)

	add, err := NewProjectEvent(device.ID, EventProjectAdded, 10<<hlcLogicalBits, ProjectPayload{
		Path: "work/acme/api", Type: "git_repo", RemoteKey: "github.com/acme/api",
	})
	if err != nil {
		t.Fatal(err)
	}
	del, err := NewProjectEvent(device.ID, EventProjectDeleted, 20<<hlcLogicalBits, ProjectPayload{Path: "work/acme/api"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEvents(ctx, st, []state.Event{add, del}); err != nil {
		t.Fatal(err)
	}
	// Below the tombstone HLC: nothing purged.
	purged, err := st.GCTombstones(ctx, 5<<hlcLogicalBits)
	if err != nil {
		t.Fatal(err)
	}
	if purged != 0 {
		t.Fatalf("GCTombstones below tombstone purged %d, want 0", purged)
	}
	// Above the tombstone HLC: purged.
	purged, err = st.GCTombstones(ctx, 30<<hlcLogicalBits)
	if err != nil {
		t.Fatal(err)
	}
	if purged != 1 {
		t.Fatalf("GCTombstones above tombstone purged %d, want 1", purged)
	}
}

func hasConflictType(conflicts []state.Conflict, typ string) bool {
	for _, c := range conflicts {
		if c.Type == typ {
			return true
		}
	}
	return false
}

func countConflictType(conflicts []state.Conflict, typ string) int {
	var count int
	for _, c := range conflicts {
		if c.Type == typ {
			count++
		}
	}
	return count
}

func signedProjectEvent(t *testing.T, signing devicekeys.SigningIdentity, deviceID string, seq, hlc int64, nsPath, remoteKey string) state.Event {
	t.Helper()
	ev, err := NewProjectEvent(deviceID, EventProjectAdded, hlc, ProjectPayload{
		Path: nsPath, Type: "git_repo", RemoteKey: remoteKey, RemoteURL: "https://example.com/" + remoteKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	ev.Seq = seq
	sig, err := devicekeys.Sign(signing.Private, "devstrap:event:v1", state.EventSignaturePayload(ev))
	if err != nil {
		t.Fatal(err)
	}
	ev.DeviceSig = sig
	return ev
}

func addRemoteDeviceForApplyTest(t *testing.T, st *state.Store, id, trustState string) devicekeys.SigningIdentity {
	t.Helper()
	signing, err := devicekeys.NewSigningIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertDevice(context.Background(), state.Device{
		ID:               id,
		Name:             id,
		OS:               "linux",
		Arch:             "arm64",
		SigningPublicKey: signing.Public,
		TrustState:       trustState,
	}); err != nil {
		t.Fatal(err)
	}
	return signing
}

// projEvent builds a project.added/updated event at the given (unshifted) HLC.
func projEvent(t *testing.T, dev, typ string, hlc int64, nsPath, key string) state.Event {
	t.Helper()
	p := ProjectPayload{Path: nsPath, Type: "git_repo"}
	if key != "" {
		p.RemoteKey = key
		p.RemoteURL = "https://example.com/" + key
	}
	ev, err := NewProjectEvent(dev, typ, hlc<<hlcLogicalBits, p)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func TestApplyEventsQuarantinesVerificationFailureAndAdvancesCursor(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	cSigning := addRemoteDeviceForApplyTest(t, st, "device-c", "approved")
	bSigning := addRemoteDeviceForApplyTest(t, st, "device-b", "revoked")

	validC1 := signedProjectEvent(t, cSigning, "device-c", 1, 10<<hlcLogicalBits, "work/acme/c1", "github.com/acme/c1")
	revokedB1 := signedProjectEvent(t, bSigning, "device-b", 1, 20<<hlcLogicalBits, "work/acme/b1", "github.com/acme/b1")
	validC2 := signedProjectEvent(t, cSigning, "device-c", 2, 30<<hlcLogicalBits, "work/acme/c2", "github.com/acme/c2")

	safeCursor, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{validC1, revokedB1, validC2})
	if err != nil {
		t.Fatalf("ApplyEvents should quarantine verification failure and continue: %v", err)
	}
	if safeCursor != 30<<hlcLogicalBits {
		t.Fatalf("safeCursor = %d, want %d", safeCursor, 30<<hlcLogicalBits)
	}
	// P6-HUB-01: the quarantine must be visible to callers (gc gate). The
	// verification quarantine is permanent, so it does not hold the cursor.
	if stats.Quarantined != 1 || stats.CursorHeld {
		t.Fatalf("stats = %+v, want Quarantined=1 CursorHeld=false", stats)
	}
	projection := projectionOf(t, st)
	if _, ok := projection["work/acme/c1"]; !ok {
		t.Fatalf("work/acme/c1 missing from projection: %+v", projection)
	}
	if _, ok := projection["work/acme/c2"]; !ok {
		t.Fatalf("work/acme/c2 missing from projection: %+v", projection)
	}
	if _, ok := projection["work/acme/b1"]; ok {
		t.Fatalf("revoked device event applied unexpectedly: %+v", projection)
	}
	conflicts, err := st.OpenConflicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := countConflictType(conflicts, ConflictEventVerification); got != 1 {
		t.Fatalf("event verification conflicts = %d, want 1: %+v", got, conflicts)
	}
	var details eventVerificationConflictDetails
	if err := json.Unmarshal([]byte(conflicts[0].DetailsJSON), &details); err != nil {
		t.Fatal(err)
	}
	if details.EventID != revokedB1.ID || details.DeviceID != revokedB1.DeviceID || details.EventJSON == "" {
		t.Fatalf("conflict details = %+v, want revoked event identity and full event json", details)
	}
	if details.Kind != EventConflictKindVerification {
		t.Fatalf("details.Kind = %q, want %q", details.Kind, EventConflictKindVerification)
	}
	var replayEvent state.Event
	if err := json.Unmarshal([]byte(details.EventJSON), &replayEvent); err != nil {
		t.Fatal(err)
	}
	if replayEvent.ID != revokedB1.ID || replayEvent.PayloadJSON != revokedB1.PayloadJSON || replayEvent.DeviceSig != revokedB1.DeviceSig {
		t.Fatalf("replay event = %+v, want full original event %+v", replayEvent, revokedB1)
	}
}

// A batch ENDING in a quarantined event must still advance the cursor past it:
// the pull boundary is inclusive, so holding at the last applied HLC would
// re-deliver (and re-process) the poisoned suffix on every subsequent sync.
func TestApplyEventsQuarantinedTrailingEventAdvancesCursor(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	cSigning := addRemoteDeviceForApplyTest(t, st, "device-c", "approved")
	bSigning := addRemoteDeviceForApplyTest(t, st, "device-b", "revoked")

	validC1 := signedProjectEvent(t, cSigning, "device-c", 1, 10<<hlcLogicalBits, "work/acme/c1", "github.com/acme/c1")
	revokedB1 := signedProjectEvent(t, bSigning, "device-b", 1, 20<<hlcLogicalBits, "work/acme/b1", "github.com/acme/b1")

	safeCursor, err := ApplyEvents(ctx, st, []state.Event{validC1, revokedB1})
	if err != nil {
		t.Fatalf("ApplyEvents should quarantine trailing verification failure: %v", err)
	}
	if safeCursor != 20<<hlcLogicalBits {
		t.Fatalf("safeCursor = %d, want %d (must pass the quarantined trailing event)", safeCursor, 20<<hlcLogicalBits)
	}
}

// A revoked device that keeps pushing CHAINED events (seq N, N+1 with
// prev_event_hash linking) must not wedge the cursor: signature/trust is
// verified before the prev-hash chain check, so every event from the revoked
// device fails with the permanent verification verdict instead of the
// missing-predecessor chain break that would transiently hold the cursor
// forever (the predecessor is quarantined, never inserted).
func TestApplyEventsChainedRevokedEventsDoNotWedgeCursor(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	cSigning := addRemoteDeviceForApplyTest(t, st, "device-c", "approved")
	bSigning := addRemoteDeviceForApplyTest(t, st, "device-b", "revoked")

	revokedB1 := signedProjectEvent(t, bSigning, "device-b", 1, 20<<hlcLogicalBits, "work/acme/b1", "github.com/acme/b1")
	revokedB2, err := NewProjectEvent("device-b", EventProjectAdded, 25<<hlcLogicalBits, ProjectPayload{
		Path: "work/acme/b2", Type: "git_repo", RemoteKey: "github.com/acme/b2", RemoteURL: "https://example.com/github.com/acme/b2",
	})
	if err != nil {
		t.Fatal(err)
	}
	revokedB2.Seq = 2
	revokedB2.PrevEventHash = revokedB1.ContentHash
	sig, err := devicekeys.Sign(bSigning.Private, "devstrap:event:v1", state.EventSignaturePayload(revokedB2))
	if err != nil {
		t.Fatal(err)
	}
	revokedB2.DeviceSig = sig
	validC1 := signedProjectEvent(t, cSigning, "device-c", 1, 30<<hlcLogicalBits, "work/acme/c1", "github.com/acme/c1")

	safeCursor, err := ApplyEvents(ctx, st, []state.Event{revokedB1, revokedB2, validC1})
	if err != nil {
		t.Fatalf("ApplyEvents should quarantine both chained revoked events: %v", err)
	}
	if safeCursor != 30<<hlcLogicalBits {
		t.Fatalf("safeCursor = %d, want %d (chained revoked events must not hold the cursor)", safeCursor, 30<<hlcLogicalBits)
	}
	conflicts, err := st.OpenConflicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := countConflictType(conflicts, ConflictEventVerification); got != 2 {
		t.Fatalf("event verification conflicts = %d, want 2: %+v", got, conflicts)
	}
	if hasConflictType(conflicts, "event_hash_chain_break") {
		t.Fatalf("chained revoked event misclassified as transient hash-chain break: %+v", conflicts)
	}
	if _, ok := projectionOf(t, st)["work/acme/c1"]; !ok {
		t.Fatal("valid event after chained revoked events did not apply")
	}
}

func TestApplyEventsDivergentEventQuarantines(t *testing.T) {
	ctx := context.Background()
	st, device := newSyncStore(t)
	event, err := NewProjectEvent(device.ID, EventProjectAdded, 10<<hlcLogicalBits, ProjectPayload{
		Path: "work/acme/api", Type: "git_repo", RemoteKey: "github.com/acme/api",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEvents(ctx, st, []state.Event{event}); err != nil {
		t.Fatal(err)
	}
	divergent := event
	divergent.PayloadJSON = `{"path":"work/acme/other","type":"git_repo","remote_key":"github.com/acme/other"}`
	divergent.ContentHash = state.ContentHash(divergent.PayloadJSON)

	if _, err := ApplyEvents(ctx, st, []state.Event{divergent}); err != nil {
		t.Fatalf("ApplyEvents should quarantine divergent duplicate and continue: %v", err)
	}
	conflicts, err := st.OpenConflicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := countConflictType(conflicts, ConflictEventVerification); got != 1 {
		t.Fatalf("event verification conflicts = %d, want 1: %+v", got, conflicts)
	}
	var details eventVerificationConflictDetails
	for _, c := range conflicts {
		if c.Type == ConflictEventVerification {
			if err := json.Unmarshal([]byte(c.DetailsJSON), &details); err != nil {
				t.Fatal(err)
			}
		}
	}
	if details.Kind != EventConflictKindDivergent {
		t.Fatalf("details.Kind = %q, want %q (divergent duplicates must not be approval-replayable)", details.Kind, EventConflictKindDivergent)
	}
}

func TestApplyEventsVerificationConflictDedups(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	cSigning := addRemoteDeviceForApplyTest(t, st, "device-c", "approved")
	bSigning := addRemoteDeviceForApplyTest(t, st, "device-b", "revoked")

	validC := signedProjectEvent(t, cSigning, "device-c", 1, 10<<hlcLogicalBits, "work/acme/c", "github.com/acme/c")
	revokedB := signedProjectEvent(t, bSigning, "device-b", 1, 20<<hlcLogicalBits, "work/acme/b", "github.com/acme/b")
	batch := []state.Event{validC, revokedB}
	if _, err := ApplyEvents(ctx, st, batch); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if _, err := ApplyEvents(ctx, st, batch); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	conflicts, err := st.OpenConflicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := countConflictType(conflicts, ConflictEventVerification); got != 1 {
		t.Fatalf("event verification conflicts = %d, want 1: %+v", got, conflicts)
	}

	// Dedup is by event ID, not exact details: the same event failing again
	// with a DIFFERENT error (trust state changed revoked -> lost) must not
	// open a second conflict row.
	if err := st.UpsertDevice(ctx, state.Device{
		ID: "device-b", Name: "device-b", OS: "linux", Arch: "arm64",
		SigningPublicKey: bSigning.Public, TrustState: "lost",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEvents(ctx, st, batch); err != nil {
		t.Fatalf("third apply: %v", err)
	}
	conflicts, err = st.OpenConflicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := countConflictType(conflicts, ConflictEventVerification); got != 1 {
		t.Fatalf("event verification conflicts after error-text change = %d, want 1: %+v", got, conflicts)
	}
}

// projectionOf returns path -> remote_key for the active namespace projection.
func projectionOf(t *testing.T, st *state.Store) map[string]string {
	t.Helper()
	projects, err := st.ListProjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m := make(map[string]string, len(projects))
	for _, p := range projects {
		m[p.Path] = p.RemoteKey
	}
	return m
}

// P5-SYNC-03: a rename leaves a tombstone at the old path. A stale or
// cross-batch add/update at the old path (lower HLC than the rename) must NOT
// resurrect the renamed-away project, while a legitimately newer event re-creates
// it.
func TestApplyEventsRenameLeavesTombstone(t *testing.T) {
	ctx := context.Background()
	st, device := newSyncStore(t)
	dev := device.ID // a known local device, so the rename signature gate passes

	// Batch 1: add work/x @10, then rename work/x -> work/y @20.
	if _, err := ApplyEvents(ctx, st, []state.Event{
		projEvent(t, dev, EventProjectAdded, 10, "work/x", "github.com/acme/x"),
		renameEvent(t, dev, 20<<hlcLogicalBits, "work/x", "work/y"),
	}); err != nil {
		t.Fatalf("apply batch 1: %v", err)
	}

	// Batch 2: a stale update at the OLD path (15 < rename 20) must be a no-op.
	if _, err := ApplyEvents(ctx, st, []state.Event{
		projEvent(t, dev, EventProjectUpdated, 15, "work/x", "github.com/acme/x"),
	}); err != nil {
		t.Fatalf("apply stale update: %v", err)
	}
	proj := projectionOf(t, st)
	if _, ok := proj["work/x"]; ok {
		t.Fatalf("stale update resurrected work/x: %+v", proj)
	}
	if _, ok := proj["work/y"]; !ok {
		t.Fatalf("renamed work/y missing: %+v", proj)
	}

	// Batch 3: a legitimately newer add at the old path (30 > rename 20)
	// re-creates work/x.
	if _, err := ApplyEvents(ctx, st, []state.Event{
		projEvent(t, dev, EventProjectAdded, 30, "work/x", "github.com/acme/x2"),
	}); err != nil {
		t.Fatalf("apply newer add: %v", err)
	}
	if got := projectionOf(t, st)["work/x"]; got != "github.com/acme/x2" {
		t.Fatalf("newer add failed to re-create work/x: got %q", got)
	}
}

// P5-SYNC-02: conflict.resolved matches on (type, details_json) only. Here the
// local conflict row carries a REAL, non-empty namespace_id (a per-device
// prj_<uuid>) while the remote resolution event carries a DIFFERENT namespace_id;
// the local row must still resolve so the open-conflict count converges.
func TestApplyConflictResolvedConvergesWithMismatchedNamespaceID(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)

	// Create a project so we have a real namespace_id to attach the conflict to.
	if _, err := ApplyEvents(ctx, st, []state.Event{
		projEvent(t, "device-a", EventProjectAdded, 10, "work/acme/api", "github.com/acme/api"),
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	proj, err := st.ProjectByPath(ctx, "work/acme/api")
	if err != nil {
		t.Fatal(err)
	}
	localNamespaceID := proj.ID
	if localNamespaceID == "" {
		t.Fatal("expected a non-empty local namespace id")
	}

	const (
		cType   = ConflictSamePathDifferentRemote
		details = `{"path":"work/acme/api","winner_key":"github.com/acme/api"}`
	)
	if err := st.InsertConflict(ctx, localNamespaceID, cType, details); err != nil {
		t.Fatal(err)
	}
	open, err := st.OpenConflicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("open conflicts = %d, want 1", len(open))
	}

	// Remote device resolves with a DIFFERENT namespace_id (its own prj_ id).
	payload, err := json.Marshal(ConflictResolvedPayload{
		ConflictID:  "cnf_remote",
		NamespaceID: "prj_some_other_device_id",
		Type:        cType,
		DetailsJSON: details,
		Action:      "keep-local",
	})
	if err != nil {
		t.Fatal(err)
	}
	eid, err := id.New("evt")
	if err != nil {
		t.Fatal(err)
	}
	resolved := state.Event{
		ID:          eid,
		DeviceID:    "device-b",
		HLC:         20 << hlcLogicalBits,
		Type:        EventConflictResolved,
		PayloadJSON: string(payload),
		ContentHash: state.ContentHash(string(payload)),
	}
	if _, err := ApplyEvents(ctx, st, []state.Event{resolved}); err != nil {
		t.Fatalf("apply conflict.resolved: %v", err)
	}
	if remaining, _ := st.OpenConflicts(ctx); len(remaining) != 0 {
		t.Fatalf("open conflicts after resolve = %d, want 0 (mismatched namespace_id must not block convergence)", len(remaining))
	}
}

// SYNC-01: the sync cursor is a low-water mark. A transiently-skipped event
// (here a hash-chain break) with a LOWER HLC than a valid event from another
// device must hold the returned safe cursor below it, so it is re-delivered
// next pull instead of being permanently stranded once the higher-HLC event
// advances the cursor past it.
func TestApplyEventsLowWaterMarkCursorHoldsBelowSkippedEvent(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)

	// device-x: event at a low HLC with a bogus prev_event_hash → hash-chain
	// break, never inserted.
	broken, err := NewProjectEvent("device-x", EventProjectAdded, 10<<hlcLogicalBits, ProjectPayload{
		Path: "work/acme/broken", Type: "git_repo", RemoteKey: "github.com/acme/broken",
	})
	if err != nil {
		t.Fatal(err)
	}
	broken.PrevEventHash = "sha256:bogus"

	// device-b: valid first event at a higher HLC → applied.
	valid, err := NewProjectEvent("device-b", EventProjectAdded, 20<<hlcLogicalBits, ProjectPayload{
		Path: "work/acme/valid", Type: "git_repo", RemoteKey: "github.com/acme/valid",
	})
	if err != nil {
		t.Fatal(err)
	}

	safeCursor, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{broken, valid})
	if err != nil {
		t.Fatalf("ApplyEvents should not abort on a hash-chain break: %v", err)
	}
	// P6-HUB-01: a transiently-held cursor must be visible to callers (gc gate).
	if stats.Quarantined != 1 || !stats.CursorHeld {
		t.Fatalf("stats = %+v, want Quarantined=1 CursorHeld=true", stats)
	}
	// The valid (higher-HLC) event was still applied — the batch converged.
	projects, err := st.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].Path != "work/acme/valid" {
		t.Fatalf("projects = %+v, want only the valid project applied", projects)
	}
	// SYNC-01: the safe cursor must be held below the broken event's HLC so it
	// is re-delivered next pull. Without the low-water mark the cursor would
	// advance to 20<<hlcLogicalBits and permanently strand the broken event.
	if safeCursor >= 10<<hlcLogicalBits {
		t.Fatalf("safeCursor = %d, want < %d (held below the skipped event)", safeCursor, 10<<hlcLogicalBits)
	}
	// A hash-chain conflict was recorded (deduped on re-delivery).
	conflicts, err := st.OpenConflicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !hasConflictType(conflicts, "event_hash_chain_break") {
		t.Fatalf("conflicts = %+v, want event_hash_chain_break", conflicts)
	}
}

// SYNC-01: a permanently-invalid event (HLC <= 0) is quarantined but does NOT
// hold back the cursor — it will never be re-applied, so holding at a
// non-positive cursor would strand every higher event.
func TestApplyEventsPermanentInvalidDoesNotHoldCursor(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)

	// device-x: permanently-invalid event (HLC = 0) → quarantined, not applied.
	poison, err := NewProjectEvent("device-x", EventProjectAdded, 0, ProjectPayload{
		Path: "work/acme/poison", Type: "git_repo", RemoteKey: "github.com/acme/poison",
	})
	if err != nil {
		t.Fatal(err)
	}
	// device-b: valid event at a small positive HLC → applied.
	valid, err := NewProjectEvent("device-b", EventProjectAdded, 10<<hlcLogicalBits, ProjectPayload{
		Path: "work/acme/valid", Type: "git_repo", RemoteKey: "github.com/acme/valid",
	})
	if err != nil {
		t.Fatal(err)
	}

	safeCursor, err := ApplyEvents(ctx, st, []state.Event{poison, valid})
	if err != nil {
		t.Fatalf("ApplyEvents should not abort on a quarantined event: %v", err)
	}
	// The valid event was applied and the cursor advanced to its HLC — the
	// permanently-invalid HLC=0 event did not hold it back.
	if safeCursor != 10<<hlcLogicalBits {
		t.Fatalf("safeCursor = %d, want %d (cursor not held by permanent-invalid event)", safeCursor, 10<<hlcLogicalBits)
	}
}

// PROD-06: a remote conflict.resolved event marks the matching open conflict
// row resolved on the receiving device so the open-conflict count converges
// across devices. Matching is by the stable (namespace_id, type, details_json)
// fingerprint, NOT the per-device conflict id.
func TestApplyConflictResolvedEventMarksRowResolved(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)

	// Seed an open conflict as it would exist on the receiving device.
	// Seed an open conflict as it would exist on the receiving device. An
	// empty namespace_id is allowed (the column is nullable) and exercises the
	// COALESCE fingerprint match without needing a namespace_entries row.
	const (
		nsID    = ""
		cType   = "same_path_different_remote"
		details = `{"path":"work/acme/api"}`
	)
	if err := st.InsertConflict(ctx, nsID, cType, details); err != nil {
		t.Fatal(err)
	}
	open, err := st.OpenConflicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("open conflicts before apply = %d, want 1", len(open))
	}
	localID := open[0].ID

	// A remote device emits conflict.resolved with ITS OWN conflict id (which
	// differs from the local id) but the same stable fingerprint.
	payload, err := json.Marshal(ConflictResolvedPayload{
		ConflictID:  "cnf_remote_different_id",
		NamespaceID: nsID,
		Type:        cType,
		DetailsJSON: details,
		Action:      "keep-local",
	})
	if err != nil {
		t.Fatal(err)
	}
	eid, err := id.New("evt")
	if err != nil {
		t.Fatal(err)
	}
	resolved := state.Event{
		ID:          eid,
		DeviceID:    "device-b",
		HLC:         10 << hlcLogicalBits,
		Type:        EventConflictResolved,
		PayloadJSON: string(payload),
		ContentHash: state.ContentHash(string(payload)),
	}
	if _, err := ApplyEvents(ctx, st, []state.Event{resolved}); err != nil {
		t.Fatalf("ApplyEvents conflict.resolved: %v", err)
	}

	// The local row is now resolved despite the mismatched conflict id.
	got, err := st.ConflictByID(ctx, localID)
	if err != nil {
		t.Fatalf("ConflictByID: %v", err)
	}
	if got.Status != "resolved" {
		t.Fatalf("conflict status = %q, want resolved", got.Status)
	}
	if open, _ := st.OpenConflicts(ctx); len(open) != 0 {
		t.Fatalf("open conflicts after apply = %d, want 0", len(open))
	}

	// A duplicate conflict.resolved event for the already-resolved row is an
	// idempotent no-op (does not error, does not resurrect the row).
	dup2, err := id.New("evt")
	if err != nil {
		t.Fatal(err)
	}
	dup := resolved
	dup.ID = dup2
	dup.HLC = 11 << hlcLogicalBits
	if _, err := ApplyEvents(ctx, st, []state.Event{dup}); err != nil {
		t.Fatalf("duplicate conflict.resolved should be a no-op: %v", err)
	}
	if got, _ := st.ConflictByID(ctx, localID); got.Status != "resolved" {
		t.Fatalf("conflict status after duplicate = %q, want resolved", got.Status)
	}
}

// P6-HUB-01 review: once a previously skew-quarantined event actually applies
// (local time caught up and it was re-delivered), its untrustworthy_remote_time
// conflict auto-resolves — otherwise a single transient clock-skew incident
// would block `hub gc` fleet-wide until a human ran `conflicts resolve`.
func TestApplyResolvesSkewConflictOnLateApply(t *testing.T) {
	ctx := context.Background()
	st, device := newSyncStore(t)

	event, err := NewProjectEvent(device.ID, EventProjectAdded, time.Now().UnixMilli()<<hlcLogicalBits, ProjectPayload{
		Path: "work/acme/late", Type: "git_repo", RemoteKey: "github.com/acme/late",
	})
	if err != nil {
		t.Fatal(err)
	}
	// The quarantine a previous delivery would have recorded (same stable
	// fingerprint quarantineSkewedEvent writes).
	details, err := json.Marshal(skewConflictDetails{EventID: event.ID, DeviceID: event.DeviceID, HLC: event.HLC})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertConflict(ctx, "", ConflictUntrustworthyTime, string(details)); err != nil {
		t.Fatal(err)
	}

	if _, err := ApplyEvents(ctx, st, []state.Event{event}); err != nil {
		t.Fatalf("ApplyEvents: %v", err)
	}
	conflicts, err := st.OpenConflicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if hasConflictType(conflicts, ConflictUntrustworthyTime) {
		t.Fatalf("conflicts = %+v, want the skew quarantine auto-resolved after the event applied", conflicts)
	}
}
