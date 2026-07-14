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
// skew is quarantined, not applied, holds its device's cursor for redelivery,
// and does not abort the batch.
func TestApplyEventsQuarantinesFarFutureRemoteEvent(t *testing.T) {
	ctx := context.Background()
	st, device := newSyncStore(t)

	futurePhysical := time.Now().Add(time.Hour).UnixMilli()
	poison, err := NewProjectEvent("device-future", EventProjectAdded, futurePhysical<<hlcLogicalBits, ProjectPayload{
		Path: "work/acme/poison", Type: "git_repo", RemoteKey: "github.com/acme/poison",
	})
	if err != nil {
		t.Fatal(err)
	}
	poison.Seq = 1
	good, err := NewProjectEvent(device.ID, EventProjectAdded, time.Now().UnixMilli()<<hlcLogicalBits, ProjectPayload{
		Path: "work/acme/good", Type: "git_repo", RemoteKey: "github.com/acme/good",
	})
	if err != nil {
		t.Fatal(err)
	}
	good.Seq = 1
	safe, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{poison, good}, nil)
	if err != nil {
		t.Fatalf("ApplyEvents should not abort on a quarantined event: %v", err)
	}
	if stats.Quarantined != 1 || !stats.CursorHeld {
		t.Fatalf("stats = %+v, want Quarantined=1 CursorHeld=true", stats)
	}
	if safe.After(poison.DeviceID) != 0 {
		t.Fatalf("safe = %v, want %s held at 0 below the future event", safe, poison.DeviceID)
	}
	if safe.After(good.DeviceID) != 1 {
		t.Fatalf("safe = %v, want %s advanced to 1", safe, good.DeviceID)
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

// P4-SYNC-03: an event whose physical HLC predates the DevStrap launch epoch is
// permanently quarantined and consumed so it cannot claim a namespace path
// with an implausibly old first-writer timestamp or wedge the transport cursor.
func TestApplyEventsQuarantinesImplausiblyOldRemoteEventAndConsumesCursor(t *testing.T) {
	saved := epochFloorMS
	epochFloorMS = 1704067200000
	defer func() { epochFloorMS = saved }()

	ctx := context.Background()
	st, _ := newSyncStore(t)
	signing := addRemoteDeviceForApplyTest(t, st, "device-past", "approved")
	event := signedProjectEvent(t, signing, "device-past", 1, 1000<<hlcLogicalBits,
		"work/acme/past", "github.com/acme/past")

	safe, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{event}, nil)
	if err != nil {
		t.Fatalf("ApplyEventsWithStats: %v", err)
	}
	if stats.Quarantined != 1 || stats.CursorHeld {
		t.Fatalf("stats = %+v, want Quarantined=1 CursorHeld=false", stats)
	}
	if safe.After(event.DeviceID) != event.Seq {
		t.Fatalf("safe = %v, want %s advanced past consumed seq %d", safe, event.DeviceID, event.Seq)
	}

	projects, err := st.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 0 {
		t.Fatalf("projects = %+v, want implausibly old event not applied", projects)
	}

	wantDetails, err := json.Marshal(skewConflictDetails{
		EventID: event.ID, DeviceID: event.DeviceID, HLC: event.HLC,
	})
	if err != nil {
		t.Fatal(err)
	}
	conflicts, err := st.OpenConflicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 || conflicts[0].Type != ConflictUntrustworthyTime ||
		conflicts[0].DetailsJSON != string(wantDetails) {
		t.Fatalf("conflicts = %+v, want one open %s conflict with details %s",
			conflicts, ConflictUntrustworthyTime, wantDetails)
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
	if err := st.UpdateProjectLocalState(ctx, project.ID, "/tmp/Code/work/acme/api", "available", "dirty", ""); err != nil {
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

// A delete strictly below the live add's source-event HLC is stale and must be
// a no-op — in BOTH pull-window orders. Before decideDelete gained the live-row
// HLC gate (the P5-ARCH-01 review finding), the A-then-D order destroyed the
// newer add while D-then-A kept it: a real convergence divergence across
// separate pull windows. Equal HLC still resolves in the delete's favor, in
// both orders, mirroring importTombstoneTx exactly.
func TestApplyEventsStaleDeleteDoesNotDestroyNewerAdd(t *testing.T) {
	ctx := context.Background()

	newAdd := func(t *testing.T, dev string, hlc int64) state.Event {
		t.Helper()
		add, err := NewProjectEvent(dev, EventProjectAdded, hlc, ProjectPayload{
			Path: "work/acme/api", Type: "git_repo", RemoteKey: "github.com/acme/api",
		})
		if err != nil {
			t.Fatal(err)
		}
		return add
	}
	newDel := func(t *testing.T, dev string, hlc int64) state.Event {
		t.Helper()
		del, err := NewProjectEvent(dev, EventProjectDeleted, hlc, ProjectPayload{Path: "work/acme/api"})
		if err != nil {
			t.Fatal(err)
		}
		return del
	}
	terminalHLC := func(t *testing.T, st *state.Store) int64 {
		t.Helper()
		p, err := st.ProjectByPath(ctx, "work/acme/api")
		if err != nil {
			t.Fatalf("work/acme/api must stay active: %v", err)
		}
		return p.SourceEventHLC
	}

	// Order 1 (the pre-fix loss): add@10 applies, then the stale delete@5
	// arrives in a LATER pull window.
	stA, devA := newSyncStore(t)
	if _, err := ApplyEvents(ctx, stA, []state.Event{newAdd(t, devA.ID, 10<<hlcLogicalBits)}); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEvents(ctx, stA, []state.Event{newDel(t, devA.ID, 5<<hlcLogicalBits)}); err != nil {
		t.Fatal(err)
	}

	// Order 2: delete@5 tombstones first, then the newer add@10 arrives.
	stB, devB := newSyncStore(t)
	if _, err := ApplyEvents(ctx, stB, []state.Event{newDel(t, devB.ID, 5<<hlcLogicalBits)}); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEvents(ctx, stB, []state.Event{newAdd(t, devB.ID, 10<<hlcLogicalBits)}); err != nil {
		t.Fatal(err)
	}

	if a, b := terminalHLC(t, stA), terminalHLC(t, stB); a != b || a != 10<<hlcLogicalBits {
		t.Fatalf("pull-window orders diverged: add-then-delete=%d delete-then-add=%d, want both %d", a, b, int64(10)<<hlcLogicalBits)
	}

	// Dirty + strictly-newer add: the stale delete is a no-op BEFORE the dirty
	// guard (matching importTombstoneTx's precedence) — the row is kept and NO
	// pending_delete_conflict is raised, since the delete lost on LWW.
	stDirty, devDirty := newSyncStore(t)
	if _, err := ApplyEvents(ctx, stDirty, []state.Event{newAdd(t, devDirty.ID, 10<<hlcLogicalBits)}); err != nil {
		t.Fatal(err)
	}
	dirtyProject, err := stDirty.ProjectByPath(ctx, "work/acme/api")
	if err != nil {
		t.Fatal(err)
	}
	if err := stDirty.UpdateProjectLocalState(ctx, dirtyProject.ID, "/tmp/Code/work/acme/api", "available", "dirty", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEvents(ctx, stDirty, []state.Event{newDel(t, devDirty.ID, 5<<hlcLogicalBits)}); err != nil {
		t.Fatal(err)
	}
	if got := terminalHLC(t, stDirty); got != 10<<hlcLogicalBits {
		t.Fatalf("dirty row with a newer add must survive a stale delete: source_hlc=%d", got)
	}
	dirtyConflicts, err := stDirty.OpenConflicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if hasConflictType(dirtyConflicts, "pending_delete_conflict") {
		t.Fatalf("a stale delete must not raise pending_delete_conflict on a newer dirty row: %+v", dirtyConflicts)
	}

	// Equal-HLC tie: the delete wins in BOTH orders (the bare-HLC rule shared
	// with importTombstoneTx; a full-coordinate tie-break would diverge).
	for name, order := range map[string][2]string{"add-then-delete": {"add", "del"}, "delete-then-add": {"del", "add"}} {
		st, dev := newSyncStore(t)
		for _, kind := range order {
			var ev state.Event
			if kind == "add" {
				ev = newAdd(t, dev.ID, 7<<hlcLogicalBits)
			} else {
				ev = newDel(t, dev.ID, 7<<hlcLogicalBits)
			}
			if _, err := ApplyEvents(ctx, st, []state.Event{ev}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := st.ProjectByPath(ctx, "work/acme/api"); err == nil {
			t.Fatalf("%s: equal-HLC add+delete must converge on deleted", name)
		}
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

func envProfileEvent(t *testing.T, id, dev string, seq, hlc int64, payload EnvProfilePayload) state.Event {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return state.Event{
		ID:          id,
		DeviceID:    dev,
		Seq:         seq,
		HLC:         hlc << hlcLogicalBits,
		Type:        EventEnvProfileUpdated,
		PayloadJSON: string(raw),
		ContentHash: state.ContentHash(string(raw)),
	}
}

func signedProjEvent(t *testing.T, signing devicekeys.SigningIdentity, id, dev string, seq, hlc int64, typ, nsPath, key string) state.Event {
	t.Helper()
	ev := projEvent(t, dev, typ, hlc, nsPath, key)
	ev.ID = id
	ev.Seq = seq
	sig, err := devicekeys.Sign(signing.Private, "devstrap:event:v2", state.EventSignaturePayloadV2(ev))
	if err != nil {
		t.Fatal(err)
	}
	ev.DeviceSig = sig
	return ev
}

func signedEnvProfileEvent(t *testing.T, signing devicekeys.SigningIdentity, id, dev string, seq, hlc int64, payload EnvProfilePayload) state.Event {
	t.Helper()
	ev := envProfileEvent(t, id, dev, seq, hlc, payload)
	sig, err := devicekeys.Sign(signing.Private, "devstrap:event:v2", state.EventSignaturePayloadV2(ev))
	if err != nil {
		t.Fatal(err)
	}
	ev.DeviceSig = sig
	return ev
}

func draftSnapshotEvent(t *testing.T, id, dev string, seq, hlc int64, payload DraftSnapshotPayload) state.Event {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return state.Event{
		ID:          id,
		DeviceID:    dev,
		Seq:         seq,
		HLC:         hlc << hlcLogicalBits,
		Type:        EventDraftSnapshotCreated,
		PayloadJSON: string(raw),
		ContentHash: state.ContentHash(string(raw)),
	}
}

func signedDraftSnapshotEvent(t *testing.T, signing devicekeys.SigningIdentity, id, dev string, seq, hlc int64, payload DraftSnapshotPayload) state.Event {
	t.Helper()
	ev := draftSnapshotEvent(t, id, dev, seq, hlc, payload)
	sig, err := devicekeys.Sign(signing.Private, "devstrap:event:v2", state.EventSignaturePayloadV2(ev))
	if err != nil {
		t.Fatal(err)
	}
	ev.DeviceSig = sig
	return ev
}

func TestApplyEnvProfileEventCreatesProfileAndBindings(t *testing.T) {
	ctx := context.Background()
	st, device := newSyncStore(t)
	signing := addRemoteDeviceForApplyTest(t, st, "device-env", "approved")
	now := time.Now().UnixMilli()
	add := projEvent(t, device.ID, EventProjectAdded, now, "work/acme/api", "github.com/acme/api")
	env := signedEnvProfileEvent(t, signing, "evt_env", "device-env", 1, now+1, EnvProfilePayload{
		Path:     "work/acme/api",
		Profile:  "default",
		Provider: "devstrap_encrypted",
		Mode:     "hydrate_or_runtime",
		BlobRef:  "age_blob:deadbeef",
		VarNames: []string{"API_TOKEN", "DB_URL"},
	})
	if _, err := ApplyEvents(ctx, st, []state.Event{add, env}); err != nil {
		t.Fatal(err)
	}
	project, err := st.ProjectByPath(ctx, "work/acme/api")
	if err != nil {
		t.Fatal(err)
	}
	profile, bindings, err := st.EnvProfileForProject(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Provider != "devstrap_encrypted" || len(bindings) != 2 || bindings[0].EncryptedValueRef != "age_blob:deadbeef" {
		t.Fatalf("profile=%#v bindings=%#v", profile, bindings)
	}
}

func TestApplyEnvProfileEventDuplicateIdempotent(t *testing.T) {
	ctx := context.Background()
	st, device := newSyncStore(t)
	signing := addRemoteDeviceForApplyTest(t, st, "device-env", "approved")
	now := time.Now().UnixMilli()
	add := projEvent(t, device.ID, EventProjectAdded, now, "work/acme/api", "github.com/acme/api")
	env := signedEnvProfileEvent(t, signing, "evt_env", "device-env", 1, now+1, EnvProfilePayload{
		Path:     "work/acme/api",
		Profile:  "default",
		Provider: "devstrap_encrypted",
		Mode:     "hydrate_or_runtime",
		BlobRef:  "age_blob:deadbeef",
		VarNames: []string{"API_TOKEN"},
	})
	if _, err := ApplyEvents(ctx, st, []state.Event{add, env}); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEvents(ctx, st, []state.Event{env}); err != nil {
		t.Fatal(err)
	}
	project, err := st.ProjectByPath(ctx, "work/acme/api")
	if err != nil {
		t.Fatal(err)
	}
	_, bindings, err := st.EnvProfileForProject(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0].VarName != "API_TOKEN" {
		t.Fatalf("bindings=%#v, want one idempotent binding", bindings)
	}
}

// TestApplyEnvProfileEventUnknownProjectQuarantinesWithoutAbort: an env event
// for an absent (NOT tombstoned) project must not abort the batch, must not be
// silently consumed, and must leave a replayable env_pending_project
// quarantine; ReplayPendingProjectConflicts recovers it once the project
// applies (Codex review P2 on the original soft-skip).
func TestApplyEnvProfileEventUnknownProjectQuarantinesWithoutAbort(t *testing.T) {
	ctx := context.Background()
	st, device := newSyncStore(t)
	signing := addRemoteDeviceForApplyTest(t, st, "device-env", "approved")
	now := time.Now().UnixMilli()
	env := signedEnvProfileEvent(t, signing, "evt_env_missing", "device-env", 1, now, EnvProfilePayload{
		Path:     "work/acme/missing",
		Profile:  "default",
		Provider: "devstrap_encrypted",
		Mode:     "hydrate_or_runtime",
		BlobRef:  "age_blob:deadbeef",
		VarNames: []string{"API_TOKEN"},
	})
	add := projEvent(t, device.ID, EventProjectAdded, now+1, "work/acme/valid", "github.com/acme/valid")
	add.Seq = 1
	safe, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{env, add}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Quarantined != 1 || stats.CursorHeld {
		t.Fatalf("stats=%+v, want exactly the env event quarantined and no held cursor", stats)
	}
	if safe.After("device-env") != 1 || safe.After(device.ID) != 1 {
		t.Fatalf("safe cursor=%v, want both devices advanced (quarantine consumes the slot)", safe)
	}
	if _, err := st.ProjectByPath(ctx, "work/acme/valid"); err != nil {
		t.Fatal(err)
	}
	conflicts, err := st.OpenConflictsByType(ctx, ConflictEventVerification)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range conflicts {
		var d eventVerificationConflictDetails
		if json.Unmarshal([]byte(c.DetailsJSON), &d) == nil && d.Kind == EventConflictKindEnvPendingProject && d.EventID == "evt_env_missing" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want an env_pending_project quarantine for evt_env_missing, got %#v", conflicts)
	}

	// Replay before the project exists: row stays open, nothing recovered.
	if n, err := ReplayPendingProjectConflicts(ctx, st); err != nil || n != 0 {
		t.Fatalf("premature replay: n=%d err=%v", n, err)
	}

	// The missing project arrives (same origin device as the env event, next
	// seq), then the per-cycle replay recovers the profile and resolves the
	// quarantine.
	addMissing := signedProjEvent(t, signing, "evt_add_missing", "device-env", 2, now+2, EventProjectAdded, "work/acme/missing", "github.com/acme/missing")
	if _, err := ApplyEvents(ctx, st, []state.Event{addMissing}); err != nil {
		t.Fatal(err)
	}
	if n, err := ReplayPendingProjectConflicts(ctx, st); err != nil || n != 1 {
		t.Fatalf("replay after project applied: n=%d err=%v", n, err)
	}
	project, err := st.ProjectByPath(ctx, "work/acme/missing")
	if err != nil {
		t.Fatal(err)
	}
	profile, bindings, err := st.EnvProfileForProject(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Provider != "devstrap_encrypted" || len(bindings) != 1 || bindings[0].EncryptedValueRef != "age_blob:deadbeef" {
		t.Fatalf("recovered profile=%#v bindings=%#v", profile, bindings)
	}
	open, err := st.OpenConflictsByType(ctx, ConflictEventVerification)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range open {
		var d eventVerificationConflictDetails
		if json.Unmarshal([]byte(c.DetailsJSON), &d) == nil && d.Kind == EventConflictKindEnvPendingProject {
			t.Fatalf("env_pending_project conflict should be resolved after replay: %#v", c)
		}
	}
}

// TestApplyEnvProfileEventTombstonedProjectDrops: a winning delete drops the
// env pointer silently — no quarantine, no batch abort (a re-add + re-capture
// is the documented recovery).
func TestApplyEnvProfileEventTombstonedProjectDrops(t *testing.T) {
	ctx := context.Background()
	st, device := newSyncStore(t)
	signing := addRemoteDeviceForApplyTest(t, st, "device-env", "approved")
	now := time.Now().UnixMilli()
	add := projEvent(t, device.ID, EventProjectAdded, now, "work/acme/gone", "github.com/acme/gone")
	del := projEvent(t, device.ID, EventProjectDeleted, now+1, "work/acme/gone", "github.com/acme/gone")
	if _, err := ApplyEvents(ctx, st, []state.Event{add, del}); err != nil {
		t.Fatal(err)
	}
	env := signedEnvProfileEvent(t, signing, "evt_env_gone", "device-env", 1, now+2, EnvProfilePayload{
		Path:     "work/acme/gone",
		Profile:  "default",
		Provider: "devstrap_encrypted",
		Mode:     "hydrate_or_runtime",
		BlobRef:  "age_blob:deadbeef",
		VarNames: []string{"API_TOKEN"},
	})
	_, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{env}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Quarantined != 0 {
		t.Fatalf("stats=%+v, want the tombstoned env pointer dropped without quarantine", stats)
	}
	conflicts, err := st.OpenConflictsByType(ctx, ConflictEventVerification)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range conflicts {
		var d eventVerificationConflictDetails
		if json.Unmarshal([]byte(c.DetailsJSON), &d) == nil && d.Kind == EventConflictKindEnvPendingProject {
			t.Fatalf("tombstoned drop must not quarantine: %#v", c)
		}
	}
}

func TestApplyDraftSnapshotUnknownProjectQuarantinesWithoutAbort(t *testing.T) {
	ctx := context.Background()
	st, device := newSyncStore(t)
	signing := addRemoteDeviceForApplyTest(t, st, "device-draft", "approved")
	now := time.Now().UnixMilli()
	draft := signedDraftSnapshotEvent(t, signing, "evt_draft_missing", "device-draft", 1, now, DraftSnapshotPayload{
		Path:      "work/acme/missing-draft",
		BlobRef:   "age_blob:deadbeef",
		ByteSize:  42,
		FileCount: 3,
	})
	add := projEvent(t, device.ID, EventProjectAdded, now+1, "work/acme/valid-draft", "github.com/acme/valid-draft")
	add.Seq = 1
	safe, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{draft, add}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Quarantined != 1 || stats.CursorHeld {
		t.Fatalf("stats=%+v, want exactly the draft event quarantined and no held cursor", stats)
	}
	if safe.After("device-draft") != 1 || safe.After(device.ID) != 1 {
		t.Fatalf("safe cursor=%v, want both devices advanced (quarantine consumes the slot)", safe)
	}
	if _, err := st.ProjectByPath(ctx, "work/acme/valid-draft"); err != nil {
		t.Fatal(err)
	}
	conflicts, err := st.OpenConflictsByType(ctx, ConflictEventVerification)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range conflicts {
		var d eventVerificationConflictDetails
		if json.Unmarshal([]byte(c.DetailsJSON), &d) == nil && d.Kind == EventConflictKindDraftPendingProject && d.EventID == "evt_draft_missing" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want a draft_pending_project quarantine for evt_draft_missing, got %#v", conflicts)
	}
}

func TestApplyDraftSnapshotTombstonedProjectDrops(t *testing.T) {
	ctx := context.Background()
	st, device := newSyncStore(t)
	signing := addRemoteDeviceForApplyTest(t, st, "device-draft", "approved")
	now := time.Now().UnixMilli()
	add := projEvent(t, device.ID, EventProjectAdded, now, "work/acme/gone-draft", "github.com/acme/gone-draft")
	del := projEvent(t, device.ID, EventProjectDeleted, now+1, "work/acme/gone-draft", "github.com/acme/gone-draft")
	if _, err := ApplyEvents(ctx, st, []state.Event{add, del}); err != nil {
		t.Fatal(err)
	}
	draft := signedDraftSnapshotEvent(t, signing, "evt_draft_gone", "device-draft", 1, now+2, DraftSnapshotPayload{
		Path:      "work/acme/gone-draft",
		BlobRef:   "age_blob:deadbeef",
		ByteSize:  42,
		FileCount: 3,
	})
	_, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{draft}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Quarantined != 0 {
		t.Fatalf("stats=%+v, want the tombstoned draft pointer dropped without quarantine", stats)
	}
	conflicts, err := st.OpenConflictsByType(ctx, ConflictEventVerification)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range conflicts {
		var d eventVerificationConflictDetails
		if json.Unmarshal([]byte(c.DetailsJSON), &d) == nil && d.Kind == EventConflictKindDraftPendingProject {
			t.Fatalf("tombstoned draft drop must not quarantine: %#v", c)
		}
	}
}

// TestApplyDraftSnapshotBadBlobRefQuarantinesWithoutAbort (review finding): a
// signed draft event whose project EXISTS but whose blob ref can never pass
// RecordDraftSnapshotTx's validation must quarantine-as-consumed at the apply
// layer — a raw store error would abort the batch, or error-loop the pending
// replay once the project lands.
func TestApplyDraftSnapshotBadBlobRefQuarantinesWithoutAbort(t *testing.T) {
	ctx := context.Background()
	st, device := newSyncStore(t)
	signing := addRemoteDeviceForApplyTest(t, st, "device-draft", "approved")
	now := time.Now().UnixMilli()
	add := projEvent(t, device.ID, EventProjectAdded, now, "work/acme/badref", "github.com/acme/badref")
	add.Seq = 1
	bad := signedDraftSnapshotEvent(t, signing, "evt_badref_draft", "device-draft", 1, now+1, DraftSnapshotPayload{
		Path:      "work/acme/badref",
		BlobRef:   "s3://not-an-age-blob",
		ByteSize:  1,
		FileCount: 1,
	})
	good := projEvent(t, device.ID, EventProjectAdded, now+2, "work/acme/after-badref", "github.com/acme/after-badref")
	good.Seq = 2
	_, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{add, bad, good}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Quarantined != 1 || stats.CursorHeld {
		t.Fatalf("stats=%+v, want bad blob ref quarantined as consumed", stats)
	}
	if _, err := st.ProjectByPath(ctx, "work/acme/after-badref"); err != nil {
		t.Fatalf("batch must continue past the bad blob ref: %v", err)
	}
}

// TestApplyDraftSnapshotPendingChainSuccessorRecovers (Codex review): a
// pending-quarantined pointer is consumed for the cursor but never inserted
// into events, so an approved device's NEXT chained event breaks on
// validatePrevEventHash and HOLDS that device's cursor. This pins that the
// hold is temporary, not a wedge: once the project lands, the pending replay
// inserts the pointer, the re-delivered successor applies, and its hash-chain
// conflict auto-resolves (the P6-SEC-03 resolve-by-event-id path).
func TestApplyDraftSnapshotPendingChainSuccessorRecovers(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	signingB := addRemoteDeviceForApplyTest(t, st, "device-b", "approved")
	signingC := addRemoteDeviceForApplyTest(t, st, "device-c", "approved")
	now := time.Now().UnixMilli()

	draft := signedDraftSnapshotEvent(t, signingB, "evt_chain_draft", "device-b", 1, now, DraftSnapshotPayload{
		Path:      "work/acme/chain",
		BlobRef:   "age_blob:feedface",
		ByteSize:  5,
		FileCount: 1,
	})
	successor := projEvent(t, "device-b", EventProjectAdded, now+1, "work/acme/other-b", "github.com/acme/other-b")
	successor.ID = "evt_chain_successor"
	successor.Seq = 2
	successor.PrevEventHash = draft.ContentHash
	sig, err := devicekeys.Sign(signingB.Private, "devstrap:event:v2", state.EventSignaturePayloadV2(successor))
	if err != nil {
		t.Fatal(err)
	}
	successor.DeviceSig = sig

	safe, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{draft, successor}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The pending draft is consumed; the chained successor breaks and HOLDS
	// device-b's cursor below seq 2 for re-delivery.
	if stats.Quarantined != 2 {
		t.Fatalf("stats=%+v, want pending draft + chain break both quarantined", stats)
	}
	if got := safe.After("device-b"); got != 1 {
		t.Fatalf("safe cursor for device-b = %d, want 1 (held below the chained successor)", got)
	}

	// The project lands from another device; the pending replay inserts the
	// draft pointer, restoring the chain anchor.
	add := signedProjEvent(t, signingC, "evt_chain_add", "device-c", 1, now+2, EventProjectAdded, "work/acme/chain", "github.com/acme/chain")
	if _, err := ApplyEvents(ctx, st, []state.Event{add}); err != nil {
		t.Fatal(err)
	}
	if n, err := ReplayPendingProjectConflicts(ctx, st); err != nil || n != 1 {
		t.Fatalf("pending replay: n=%d err=%v", n, err)
	}

	// The re-delivered successor now applies and auto-resolves its
	// hash-chain conflict.
	if _, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{successor}, nil); err != nil || stats.Quarantined != 0 {
		t.Fatalf("re-delivered successor: stats=%+v err=%v, want clean apply", stats, err)
	}
	if _, err := st.ProjectByPath(ctx, "work/acme/other-b"); err != nil {
		t.Fatalf("successor project missing after recovery: %v", err)
	}
	open, err := st.OpenConflictsByType(ctx, ConflictEventHashChain)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("hash-chain conflict not auto-resolved after recovery: %#v", open)
	}
}

// TestApplyEnvProfileMalformedPayloadQuarantinesWithoutAbort pins the same
// malformed-payload convention on the ENV pointer (#133 residual): a verified
// event whose payload can never decode quarantines as consumed instead of
// aborting the pull batch.
func TestApplyEnvProfileMalformedPayloadQuarantinesWithoutAbort(t *testing.T) {
	ctx := context.Background()
	st, device := newSyncStore(t)
	signing := addRemoteDeviceForApplyTest(t, st, "device-env", "approved")
	now := time.Now().UnixMilli()
	bad := state.Event{
		ID:          "evt_bad_env",
		DeviceID:    "device-env",
		Seq:         1,
		HLC:         now << hlcLogicalBits,
		Type:        EventEnvProfileUpdated,
		PayloadJSON: `{"path":`,
	}
	bad.ContentHash = state.ContentHash(bad.PayloadJSON)
	sig, err := devicekeys.Sign(signing.Private, "devstrap:event:v2", state.EventSignaturePayloadV2(bad))
	if err != nil {
		t.Fatal(err)
	}
	bad.DeviceSig = sig
	good := projEvent(t, device.ID, EventProjectAdded, now+1, "work/acme/after-env", "github.com/acme/after-env")
	good.Seq = 1
	_, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{bad, good}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Quarantined != 1 || stats.CursorHeld {
		t.Fatalf("stats=%+v, want malformed env quarantined as consumed", stats)
	}
	if _, err := st.ProjectByPath(ctx, "work/acme/after-env"); err != nil {
		t.Fatalf("batch must continue past malformed env event: %v", err)
	}
}

func TestApplyDraftSnapshotMalformedPayloadQuarantinesWithoutAbort(t *testing.T) {
	ctx := context.Background()
	st, device := newSyncStore(t)
	signing := addRemoteDeviceForApplyTest(t, st, "device-draft", "approved")
	now := time.Now().UnixMilli()
	bad := state.Event{
		ID:          "evt_bad_draft",
		DeviceID:    "device-draft",
		Seq:         1,
		HLC:         now << hlcLogicalBits,
		Type:        EventDraftSnapshotCreated,
		PayloadJSON: `{"path":`,
	}
	bad.ContentHash = state.ContentHash(bad.PayloadJSON)
	sig, err := devicekeys.Sign(signing.Private, "devstrap:event:v2", state.EventSignaturePayloadV2(bad))
	if err != nil {
		t.Fatal(err)
	}
	bad.DeviceSig = sig
	good := projEvent(t, device.ID, EventProjectAdded, now+1, "work/acme/after-draft", "github.com/acme/after-draft")
	good.Seq = 1
	_, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{bad, good}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Quarantined != 1 || stats.CursorHeld {
		t.Fatalf("stats=%+v, want malformed draft quarantined as consumed", stats)
	}
	if _, err := st.ProjectByPath(ctx, "work/acme/after-draft"); err != nil {
		t.Fatalf("batch must continue past malformed draft event: %v", err)
	}
	conflicts, err := st.OpenConflictsByType(ctx, ConflictEventVerification)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range conflicts {
		var d eventVerificationConflictDetails
		if json.Unmarshal([]byte(c.DetailsJSON), &d) == nil && d.Kind == EventConflictKindVerification && d.EventID == "evt_bad_draft" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want a verification quarantine for malformed draft payload, got %#v", conflicts)
	}
}

func TestReplayPendingDraftSnapshotConflictRecovers(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	signing := addRemoteDeviceForApplyTest(t, st, "device-draft", "approved")
	now := time.Now().UnixMilli()
	draft := signedDraftSnapshotEvent(t, signing, "evt_draft_replay", "device-draft", 1, now, DraftSnapshotPayload{
		Path:      "work/acme/replay-draft",
		BlobRef:   "age_blob:cafebabe",
		ByteSize:  99,
		FileCount: 7,
	})
	if _, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{draft}, nil); err != nil {
		t.Fatal(err)
	} else if stats.Quarantined != 1 {
		t.Fatalf("stats=%+v, want initial draft quarantine", stats)
	}
	if n, err := ReplayPendingProjectConflicts(ctx, st); err != nil || n != 0 {
		t.Fatalf("premature replay: n=%d err=%v", n, err)
	}
	add := signedProjEvent(t, signing, "evt_add_replay_draft", "device-draft", 2, now+1, EventProjectAdded, "work/acme/replay-draft", "github.com/acme/replay-draft")
	if _, err := ApplyEvents(ctx, st, []state.Event{add}); err != nil {
		t.Fatal(err)
	}
	if n, err := ReplayPendingProjectConflicts(ctx, st); err != nil || n != 1 {
		t.Fatalf("replay after project applied: n=%d err=%v", n, err)
	}
	project, err := st.ProjectByPath(ctx, "work/acme/replay-draft")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := st.LatestDraftSnapshot(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snap == nil || snap.BlobRef != "age_blob:cafebabe" || snap.ByteSize != 99 || snap.FileCount != 7 {
		t.Fatalf("recovered draft snapshot=%#v", snap)
	}
	open, err := st.OpenConflictsByType(ctx, ConflictEventVerification)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range open {
		var d eventVerificationConflictDetails
		if json.Unmarshal([]byte(c.DetailsJSON), &d) == nil && d.Kind == EventConflictKindDraftPendingProject {
			t.Fatalf("draft_pending_project conflict should be resolved after replay: %#v", c)
		}
	}
}

func TestApplyEnvProfileEventLWWConvergesBothOrders(t *testing.T) {
	ctx := context.Background()
	for _, order := range []string{"low-then-high", "high-then-low"} {
		t.Run(order, func(t *testing.T) {
			st, device := newSyncStore(t)
			signingA := addRemoteDeviceForApplyTest(t, st, "device-a", "approved")
			signingB := addRemoteDeviceForApplyTest(t, st, "device-b", "approved")
			now := time.Now().UnixMilli()
			add := projEvent(t, device.ID, EventProjectAdded, now, "work/acme/api", "github.com/acme/api")
			if _, err := ApplyEvents(ctx, st, []state.Event{add}); err != nil {
				t.Fatal(err)
			}
			low := signedEnvProfileEvent(t, signingA, "evt_env_low", "device-a", 1, now+1, EnvProfilePayload{
				Path:     "work/acme/api",
				Profile:  "default",
				Provider: "devstrap_encrypted",
				Mode:     "hydrate_or_runtime",
				BlobRef:  "age_blob:low",
				VarNames: []string{"LOW"},
			})
			high := signedEnvProfileEvent(t, signingB, "evt_env_high", "device-b", 1, now+2, EnvProfilePayload{
				Path:     "work/acme/api",
				Profile:  "default",
				Provider: "devstrap_encrypted",
				Mode:     "hydrate_or_runtime",
				BlobRef:  "age_blob:high",
				VarNames: []string{"HIGH"},
			})
			first, second := low, high
			if order == "high-then-low" {
				first, second = high, low
			}
			if _, err := ApplyEvents(ctx, st, []state.Event{first}); err != nil {
				t.Fatal(err)
			}
			if _, err := ApplyEvents(ctx, st, []state.Event{second}); err != nil {
				t.Fatal(err)
			}
			project, err := st.ProjectByPath(ctx, "work/acme/api")
			if err != nil {
				t.Fatal(err)
			}
			_, bindings, err := st.EnvProfileForProject(ctx, project.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(bindings) != 1 || bindings[0].VarName != "HIGH" || bindings[0].EncryptedValueRef != "age_blob:high" {
				t.Fatalf("bindings=%#v, want high event to win", bindings)
			}
		})
	}
}

func TestApplyEventsQuarantinesVerificationFailureAndAdvancesCursor(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	cSigning := addRemoteDeviceForApplyTest(t, st, "device-c", "approved")
	bSigning := addRemoteDeviceForApplyTest(t, st, "device-b", "revoked")

	validC1 := signedProjectEvent(t, cSigning, "device-c", 1, 10<<hlcLogicalBits, "work/acme/c1", "github.com/acme/c1")
	revokedB1 := signedProjectEvent(t, bSigning, "device-b", 1, 20<<hlcLogicalBits, "work/acme/b1", "github.com/acme/b1")
	validC2 := signedProjectEvent(t, cSigning, "device-c", 2, 30<<hlcLogicalBits, "work/acme/c2", "github.com/acme/c2")

	safeCursor, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{validC1, revokedB1, validC2}, nil)
	if err != nil {
		t.Fatalf("ApplyEvents should quarantine verification failure and continue: %v", err)
	}
	if safeCursor.After("device-c") != 2 || safeCursor.After("device-b") != 1 {
		t.Fatalf("safeCursor = %v, want device-c:2 device-b:1", safeCursor)
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
	if safeCursor.After("device-b") != 1 {
		t.Fatalf("safeCursor = %v, want device-b:1 (must consume the quarantined trailing event)", safeCursor)
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
	if safeCursor.After("device-b") != 2 || safeCursor.After("device-c") != 1 {
		t.Fatalf("safeCursor = %v, want device-b:2 device-c:1 (chained revoked events must not hold the cursor)", safeCursor)
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

// P6-SYNC-03: revoking the LAST approved device must not reopen the
// pre-enrollment bootstrap window. With only a revoked device on record (the
// post-revoke two-device state — no approved device left), a validly-signed
// non-destructive event from the revoked device, any event from an unknown
// device, a signed event from a known-but-pending device, and an unsigned
// event from a known device with no signing key must all be quarantined
// rather than applied — every fail-open branch of the pre-enrollment regime
// stays closed.
func TestApplyEventsRevokedLastDeviceStaysFailClosed(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	bSigning := addRemoteDeviceForApplyTest(t, st, "device-b", "revoked")
	pSigning := addRemoteDeviceForApplyTest(t, st, "device-p", "pending")
	if err := st.UpsertDevice(ctx, state.Device{
		ID: "device-n", Name: "device-n", OS: "linux", Arch: "arm64", TrustState: "pending",
	}); err != nil {
		t.Fatal(err)
	}

	revokedB := signedProjectEvent(t, bSigning, "device-b", 1, 10<<hlcLogicalBits, "work/acme/b1", "github.com/acme/b1")
	unknown := projEvent(t, "device-x", EventProjectAdded, 20, "work/acme/x1", "github.com/acme/x1")
	pendingP := signedProjectEvent(t, pSigning, "device-p", 1, 30<<hlcLogicalBits, "work/acme/p1", "github.com/acme/p1")
	noKeyN := projEvent(t, "device-n", EventProjectAdded, 40, "work/acme/n1", "github.com/acme/n1")

	safeCursor, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{revokedB, unknown, pendingP, noKeyN}, nil)
	if err != nil {
		t.Fatalf("ApplyEvents should quarantine post-revoke events, not abort: %v", err)
	}
	// The signed events carry seqs and consume their slots; the unsigned
	// Seq-0 events (unknown/no-key devices) cannot be cursored at all.
	if safeCursor.After("device-b") != 1 || safeCursor.After("device-p") != 1 {
		t.Fatalf("safeCursor = %v, want device-b:1 device-p:1", safeCursor)
	}
	if stats.Quarantined != 4 || stats.CursorHeld {
		t.Fatalf("stats = %+v, want Quarantined=4 CursorHeld=false", stats)
	}
	if projection := projectionOf(t, st); len(projection) != 0 {
		t.Fatalf("no event may apply after the last approved device is revoked, got %+v", projection)
	}
	conflicts, err := st.OpenConflicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := countConflictType(conflicts, ConflictEventVerification); got != 4 {
		t.Fatalf("event verification conflicts = %d, want 4: %+v", got, conflicts)
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
	broken.Seq = 1
	broken.PrevEventHash = "sha256:bogus"

	// device-b: valid first event at a higher HLC → applied.
	valid, err := NewProjectEvent("device-b", EventProjectAdded, 20<<hlcLogicalBits, ProjectPayload{
		Path: "work/acme/valid", Type: "git_repo", RemoteKey: "github.com/acme/valid",
	})
	if err != nil {
		t.Fatal(err)
	}
	valid.Seq = 1

	safeCursor, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{broken, valid}, nil)
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
	// SYNC-01/P5-SYNC-01: the hold is scoped to the offending origin device —
	// device-x's cursor stays below its broken seq 1 (re-delivered next pull),
	// while device-b's cursor still advances (per-device fault isolation).
	if safeCursor.After("device-x") != 0 {
		t.Fatalf("safeCursor = %v, want device-x held at 0 below the broken event", safeCursor)
	}
	if safeCursor.After("device-b") != 1 {
		t.Fatalf("safeCursor = %v, want device-b:1 (other devices unaffected by the hold)", safeCursor)
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
	poison.Seq = 1
	// device-b: valid event at a small positive HLC → applied.
	valid, err := NewProjectEvent("device-b", EventProjectAdded, 10<<hlcLogicalBits, ProjectPayload{
		Path: "work/acme/valid", Type: "git_repo", RemoteKey: "github.com/acme/valid",
	})
	if err != nil {
		t.Fatal(err)
	}
	valid.Seq = 1

	safeCursor, err := ApplyEvents(ctx, st, []state.Event{poison, valid})
	if err != nil {
		t.Fatalf("ApplyEvents should not abort on a quarantined event: %v", err)
	}
	// The valid event was applied and its device's cursor advanced; the
	// permanently-invalid HLC=0 event consumed its own slot instead of
	// holding anything.
	if safeCursor.After("device-b") != 1 {
		t.Fatalf("safeCursor = %v, want device-b:1 (cursor not held by permanent-invalid event)", safeCursor)
	}
	if safeCursor.After("device-x") != 1 {
		t.Fatalf("safeCursor = %v, want device-x:1 (permanent-invalid event consumes its slot)", safeCursor)
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

// TestApplyEventsQuarantinesUndecryptableCarrier is the apply half of the
// P6-SYNC-04 no-silent-loss contract: an enc.v2 carrier forwarded by
// EncryptedHub.Pull (AEAD authentication failed on every held key) is recorded
// as a permanent "undecryptable" event_verification_failure conflict, is never
// inserted into the event log, and does NOT hold the cursor — the surrounding
// good events apply and the batch converges. A re-delivery dedups onto the
// same conflict row instead of opening a second one.
func TestApplyEventsQuarantinesUndecryptableCarrier(t *testing.T) {
	ctx := context.Background()
	st, device := newSyncStore(t)

	otherWCK, _ := NewWCK()
	carrier, err := EncryptEvent(state.Event{
		ID: "evt_poison", DeviceID: "dev_remote", Seq: 1, HLC: 20 << hlcLogicalBits,
		Type:        EventProjectAdded,
		PayloadJSON: `{"path":"work/poison"}`,
		ContentHash: state.ContentHash(`{"path":"work/poison"}`),
	}, otherWCK, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate what Pull forwards: the receiving device does not hold
	// otherWCK, so the carrier reaches ApplyEvents still encrypted.
	good := projEvent(t, device.ID, EventProjectAdded, 30, "work/good", "github.com/org/good")

	safe, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{carrier, good}, nil)
	if err != nil {
		t.Fatalf("ApplyEventsWithStats: %v", err)
	}
	if stats.Quarantined != 1 {
		t.Fatalf("Quarantined = %d, want 1", stats.Quarantined)
	}
	if safe.After("dev_remote") != 1 {
		t.Fatalf("safe = %v, want dev_remote:1 (cursor advances past the permanent quarantine)", safe)
	}
	if projection := projectionOf(t, st); projection["work/good"] == "" {
		t.Fatalf("good event did not apply: %+v", projection)
	}
	conflicts, err := st.OpenConflicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, c := range conflicts {
		if c.Type != ConflictEventVerification {
			continue
		}
		var d eventVerificationConflictDetails
		if json.Unmarshal([]byte(c.DetailsJSON), &d) != nil || d.EventID != "evt_poison" {
			continue
		}
		found = true
		if d.Kind != EventConflictKindUndecryptable {
			t.Fatalf("conflict kind = %q, want %q", d.Kind, EventConflictKindUndecryptable)
		}
		if d.Type != EventEncryptedV2 {
			t.Fatalf("conflict event type = %q, want %q", d.Type, EventEncryptedV2)
		}
	}
	if !found {
		t.Fatalf("no undecryptable conflict recorded; conflicts = %+v", conflicts)
	}
	// The carrier must never enter the event log.
	if _, err := st.EventByID(ctx, "evt_poison"); err == nil {
		t.Fatal("undecryptable carrier was inserted into the event log")
	}
	// Re-delivery dedups onto the same conflict row.
	if _, _, err := ApplyEventsWithStats(ctx, st, []state.Event{carrier}, nil); err != nil {
		t.Fatalf("re-delivery: %v", err)
	}
	conflicts, err = st.OpenConflicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n := countConflictType(conflicts, ConflictEventVerification); n != 1 {
		t.Fatalf("conflict rows after re-delivery = %d, want 1 (dedup)", n)
	}
}

// TestApplyEventsSameSeqDifferentIDQuarantinesAsDivergent (post-#59 opus
// review, Major): a second event claiming an already-occupied (device, seq)
// slot under a DIFFERENT event id — a byzantine or backup-restored device
// re-minting a sequence number — must quarantine as a permanent divergence
// (consumed slot, batch continues), never surface as a raw SQL error that
// aborts the whole batch on every pull forever.
func TestApplyEventsSameSeqDifferentIDQuarantinesAsDivergent(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)

	first, err := NewProjectEvent("device-x", EventProjectAdded, 10<<hlcLogicalBits, ProjectPayload{
		Path: "work/acme/first", Type: "git_repo", RemoteKey: "github.com/acme/first",
	})
	if err != nil {
		t.Fatal(err)
	}
	first.Seq = 1
	// Same (device, seq), different id and content: an equivocation.
	equivocation, err := NewProjectEvent("device-x", EventProjectAdded, 15<<hlcLogicalBits, ProjectPayload{
		Path: "work/acme/other", Type: "git_repo", RemoteKey: "github.com/acme/other",
	})
	if err != nil {
		t.Fatal(err)
	}
	equivocation.Seq = 1
	// A later valid event from another device: the batch must keep going.
	valid, err := NewProjectEvent("device-b", EventProjectAdded, 20<<hlcLogicalBits, ProjectPayload{
		Path: "work/acme/valid", Type: "git_repo", RemoteKey: "github.com/acme/valid",
	})
	if err != nil {
		t.Fatal(err)
	}
	valid.Seq = 1

	safe, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{first, equivocation, valid}, nil)
	if err != nil {
		t.Fatalf("ApplyEvents must quarantine the equivocation, not abort: %v", err)
	}
	if stats.Quarantined != 1 || stats.CursorHeld {
		t.Fatalf("stats = %+v, want Quarantined=1 CursorHeld=false", stats)
	}
	// Both device cursors advance: the equivocation is a consumed permanent
	// quarantine at an already-consumed slot.
	if safe.After("device-x") != 1 || safe.After("device-b") != 1 {
		t.Fatalf("safe = %v, want device-x:1 device-b:1", safe)
	}
	conflicts, err := st.OpenConflicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := countConflictType(conflicts, ConflictEventVerification); got != 1 {
		t.Fatalf("verification conflicts = %d, want 1 (the divergent equivocation): %+v", got, conflicts)
	}
	projection := projectionOf(t, st)
	if _, ok := projection["work/acme/first"]; !ok {
		t.Fatalf("first occupant did not apply: %+v", projection)
	}
	if _, ok := projection["work/acme/other"]; ok {
		t.Fatalf("equivocation applied: %+v", projection)
	}
	if _, ok := projection["work/acme/valid"]; !ok {
		t.Fatalf("batch did not continue past the equivocation: %+v", projection)
	}
}

// TestApplyEventsForgedCarrierCannotAdvancePastHeldSlot (P5-SYNC-01 successor
// to the PR #44 implausible-HLC guard, which protected the retired HLC
// cursor): EVERY carrier field of an undecryptable envelope is hub-writable —
// Seq included, since AEAD authentication failed — so a forged, consumed
// carrier occupying the same (device, seq) slot as a real, transiently-held
// event must not let that device's cursor advance over the slot (held
// dominates consumed in the per-slot outcome). Otherwise a hostile hub could
// pair every held event with a forged twin and strand it forever.
func TestApplyEventsForgedCarrierCannotAdvancePastHeldSlot(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)

	// Real event from device-r at seq 1, transiently skew-held (far-future
	// HLC beyond the trusted skew).
	farFuture := (time.Now().Add(24 * time.Hour).UnixMilli()) << hlcLogicalBits
	held, err := NewProjectEvent("device-r", EventProjectAdded, farFuture, ProjectPayload{
		Path: "work/acme/held", Type: "git_repo", RemoteKey: "github.com/acme/held",
	})
	if err != nil {
		t.Fatal(err)
	}
	held.Seq = 1

	// Forged undecryptable carrier claiming the SAME (device, seq) slot.
	otherWCK, _ := NewWCK()
	carrier, err := EncryptEvent(state.Event{
		ID: "evt_forge", DeviceID: "device-r", Seq: 1, HLC: 20 << hlcLogicalBits,
		Type:        EventProjectAdded,
		PayloadJSON: `{"path":"work/forge"}`,
		ContentHash: state.ContentHash(`{"path":"work/forge"}`),
	}, otherWCK, 1)
	if err != nil {
		t.Fatal(err)
	}

	safe, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{held, carrier}, nil)
	if err != nil {
		t.Fatalf("ApplyEventsWithStats: %v", err)
	}
	if stats.Quarantined != 2 {
		t.Fatalf("Quarantined = %d, want 2 (skew hold + undecryptable)", stats.Quarantined)
	}
	if !stats.CursorHeld {
		t.Fatal("contested slot must report CursorHeld")
	}
	if safe.After("device-r") != 0 {
		t.Fatalf("safe = %v — a forged consumed carrier must not advance past the held real event", safe)
	}
}

// TestApplyEventsClearsSkipRecordOnConsume (P6-SYNC-02): a durable skip
// record clears when its event is finally consumed — on first APPLY, and on
// DEDUP (a restored hub object for an event this device already holds).
func TestApplyEventsClearsSkipRecordOnConsume(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)

	ev, err := NewProjectEvent("device-x", EventProjectAdded, 10<<hlcLogicalBits, ProjectPayload{
		Path: "work/acme/skiprec", Type: "git_repo", RemoteKey: "github.com/acme/skiprec",
	})
	if err != nil {
		t.Fatal(err)
	}
	ev.Seq = 1
	if _, err := st.NoteSkippedEvent(ctx, ev, "unknown-envelope-version"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ApplyEventsWithStats(ctx, st, []state.Event{ev}, nil); err != nil {
		t.Fatal(err)
	}
	if open, _ := st.OpenSkippedEvents(ctx); len(open) != 0 {
		t.Fatalf("skip records after apply = %+v, want cleared", open)
	}
	// Dedup path: re-note, re-deliver the SAME (already inserted) event.
	if _, err := st.NoteSkippedEvent(ctx, ev, "plaintext-anti-downgrade"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ApplyEventsWithStats(ctx, st, []state.Event{ev}, nil); err != nil {
		t.Fatal(err)
	}
	if open, _ := st.OpenSkippedEvents(ctx); len(open) != 0 {
		t.Fatalf("skip records after dedup = %+v, want cleared", open)
	}
}
