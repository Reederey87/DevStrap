package sync

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

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
	if err := ApplyEvents(ctx, st, []state.Event{poison, good}); err != nil {
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
	if err := ApplyEvents(ctx, st, []state.Event{remote}); err != nil {
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
	if err := ApplyEvents(ctx, st, []state.Event{add, ren}); err != nil {
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
	if err := ApplyEvents(ctx, st, []state.Event{addOther, collide}); err != nil {
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
	if err := ApplyEvents(ctx, st, []state.Event{add}); err != nil {
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
	if err := ApplyEvents(ctx, st, []state.Event{del}); err != nil {
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
	if err := ApplyEvents(ctx, st, []state.Event{add, del}); err != nil {
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
