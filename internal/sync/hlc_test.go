package sync

import (
	"context"
	"encoding/json"
	"path/filepath"
	stdsync "sync"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/state"
)

func TestHLCMonotonicSendReceive(t *testing.T) {
	now := time.UnixMilli(1000)
	clock := HLC{Now: func() time.Time { return now }}
	first := clock.Send()
	second := clock.Send()
	if second <= first {
		t.Fatalf("second send = %d, want > %d", second, first)
	}
	received, err := clock.Receive(second + 10)
	if err != nil {
		t.Fatal(err)
	}
	if received <= second+10 {
		t.Fatalf("receive = %d, want > remote", received)
	}
}

func TestHLCRejectsRemoteBeyondMaxSkew(t *testing.T) {
	now := time.UnixMilli(1000)
	clock := HLC{Now: func() time.Time { return now }, MaxSkew: time.Second}
	remote := pack(now.Add(2*time.Second).UnixMilli(), 0)
	if _, err := clock.Receive(remote); err == nil {
		t.Fatal("expected max skew error")
	}
}

func TestHLCLogicalCounterOverflowAdvancesPhysicalComponent(t *testing.T) {
	now := time.UnixMilli(1000)
	clock := HLC{Now: func() time.Time { return now }, Last: pack(1000, hlcLogicalMask)}
	got := clock.Send()
	physical, logical := unpack(got)
	if physical != 1001 || logical != 0 {
		t.Fatalf("overflow send unpacked to (%d,%d), want (1001,0)", physical, logical)
	}
}

func TestHLCMonotonicUnderBackwardClock(t *testing.T) {
	now := time.UnixMilli(1000)
	clock := HLC{Now: func() time.Time { return now }}
	first := clock.Send()
	now = time.UnixMilli(500) // wall clock jumps backward (NTP step / VM restore)
	second := clock.Send()
	if second <= first {
		t.Fatalf("HLC regressed under backward clock: first=%d second=%d", first, second)
	}
	physical, _ := unpack(second)
	if physical < 1000 {
		t.Fatalf("physical component regressed to %d, want >= 1000", physical)
	}
}

func TestHLCSendAdvancesPhysicalOnTick(t *testing.T) {
	now := time.UnixMilli(1000)
	clock := HLC{Now: func() time.Time { return now }}
	clock.Send()
	now = time.UnixMilli(1001)
	got := clock.Send()
	physical, logical := unpack(got)
	if physical != 1001 || logical != 0 {
		t.Fatalf("tick send unpacked to (%d,%d), want (1001,0)", physical, logical)
	}
}

func TestHLCSendIsRaceFreeAndStrictlyIncreasing(t *testing.T) {
	now := time.UnixMilli(1000)
	clock := HLC{Now: func() time.Time { return now }}
	const goroutines, perG = 16, 64
	results := make(chan int64, goroutines*perG)
	var wg stdsync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perG; j++ {
				results <- clock.Send()
			}
		}()
	}
	wg.Wait()
	close(results)
	seen := make(map[int64]struct{}, goroutines*perG)
	for v := range results {
		if _, dup := seen[v]; dup {
			t.Fatalf("duplicate HLC value %d under concurrency", v)
		}
		seen[v] = struct{}{}
	}
	if len(seen) != goroutines*perG {
		t.Fatalf("got %d unique values, want %d", len(seen), goroutines*perG)
	}
}

func TestApplyEventsIsIdempotentAndDetectsRemoteConflict(t *testing.T) {
	ctx := context.Background()
	st, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(ctx, "test", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	device, err := st.EnsureDevice(ctx, "device-a")
	if err != nil {
		t.Fatal(err)
	}
	event, err := NewProjectEvent(device.ID, EventProjectAdded, 1, ProjectPayload{
		Path:          "work/acme/api",
		Type:          "git_repo",
		RemoteURL:     "git@github.com:acme/api.git",
		RemoteKey:     "github.com/acme/api",
		DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEvents(ctx, st, []state.Event{event, event}); err != nil {
		t.Fatal(err)
	}
	projects, err := st.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 {
		t.Fatalf("projects = %d, want 1", len(projects))
	}
	conflict, err := NewProjectEvent(device.ID, EventProjectAdded, 2, ProjectPayload{
		Path:          "work/acme/api",
		Type:          "git_repo",
		RemoteURL:     "git@github.com:other/api.git",
		RemoteKey:     "github.com/other/api",
		DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEvents(ctx, st, []state.Event{conflict}); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEvents(ctx, st, []state.Event{conflict}); err != nil {
		t.Fatal(err)
	}
	projects, err = st.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].RemoteKey != "github.com/acme/api" {
		t.Fatalf("conflict overwrote project: %+v", projects)
	}
	conflicts, err := st.CountOpenConflicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if conflicts != 1 {
		t.Fatalf("open conflicts = %d, want 1", conflicts)
	}
}

func TestReconcileSamePathIsCommutative(t *testing.T) {
	acme := ProjectPayload{
		Path:          "work/acme/api",
		Type:          "git_repo",
		RemoteURL:     "git@github.com:acme/api.git",
		RemoteKey:     "github.com/acme/api",
		DefaultBranch: "main",
	}
	other := ProjectPayload{
		Path:          "work/acme/api",
		Type:          "git_repo",
		RemoteURL:     "git@github.com:other/api.git",
		RemoteKey:     "github.com/other/api",
		DefaultBranch: "main",
	}
	forwardWinner, forwardIncomingWins, forwardDetails, err := reconcileSamePath(state.ProjectStatus{
		NamespaceEntry: state.NamespaceEntry{
			ID:                  "prj_1",
			Path:                acme.Path,
			Type:                acme.Type,
			SourceEventHLC:      10,
			SourceEventDeviceID: "device-a",
			SourceEventID:       "evt_a",
		},
		RemoteURL:     acme.RemoteURL,
		RemoteKey:     acme.RemoteKey,
		DefaultBranch: acme.DefaultBranch,
	}, other, state.Event{ID: "evt_b", DeviceID: "device-b", HLC: 20})
	if err != nil {
		t.Fatal(err)
	}
	reverseWinner, reverseIncomingWins, reverseDetails, err := reconcileSamePath(state.ProjectStatus{
		NamespaceEntry: state.NamespaceEntry{
			ID:                  "prj_1",
			Path:                other.Path,
			Type:                other.Type,
			SourceEventHLC:      20,
			SourceEventDeviceID: "device-b",
			SourceEventID:       "evt_b",
		},
		RemoteURL:     other.RemoteURL,
		RemoteKey:     other.RemoteKey,
		DefaultBranch: other.DefaultBranch,
	}, acme, state.Event{ID: "evt_a", DeviceID: "device-a", HLC: 10})
	if err != nil {
		t.Fatal(err)
	}
	if forwardWinner.RemoteKey != acme.RemoteKey || reverseWinner.RemoteKey != acme.RemoteKey {
		t.Fatalf("winners = %q/%q, want %q", forwardWinner.RemoteKey, reverseWinner.RemoteKey, acme.RemoteKey)
	}
	if forwardIncomingWins || !reverseIncomingWins {
		t.Fatalf("incoming wins flags = %v/%v, want false/true", forwardIncomingWins, reverseIncomingWins)
	}
	if forwardDetails != reverseDetails {
		t.Fatalf("conflict details differ:\nforward=%s\nreverse=%s", forwardDetails, reverseDetails)
	}
}

func TestApplyEventsSamePathDifferentRemoteUsesCanonicalWinnerAcrossPullWindows(t *testing.T) {
	ctx := context.Background()
	st, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(ctx, "test", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	device, err := st.EnsureDevice(ctx, "device-a")
	if err != nil {
		t.Fatal(err)
	}
	older, err := NewProjectEvent(device.ID, EventProjectAdded, 10, ProjectPayload{
		Path:          "work/acme/api",
		Type:          "git_repo",
		RemoteURL:     "git@github.com:acme/api.git",
		RemoteKey:     "github.com/acme/api",
		DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	newer, err := NewProjectEvent(device.ID, EventProjectAdded, 20, ProjectPayload{
		Path:          "work/acme/api",
		Type:          "git_repo",
		RemoteURL:     "git@github.com:other/api.git",
		RemoteKey:     "github.com/other/api",
		DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEvents(ctx, st, []state.Event{newer}); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEvents(ctx, st, []state.Event{older}); err != nil {
		t.Fatal(err)
	}
	projects, err := st.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].RemoteKey != "github.com/acme/api" || projects[0].SourceEventID != older.ID {
		t.Fatalf("projects = %+v, want canonical older remote/event", projects)
	}
	if _, err := ApplyEvents(ctx, st, []state.Event{newer, older}); err != nil {
		t.Fatal(err)
	}
	conflicts, err := st.OpenConflicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("open conflicts = %d, want 1", len(conflicts))
	}
	var details samePathConflictDetails
	if err := json.Unmarshal([]byte(conflicts[0].DetailsJSON), &details); err != nil {
		t.Fatal(err)
	}
	if details.RemoteKeyA != "github.com/acme/api" || details.RemoteKeyB != "github.com/other/api" || details.WinnerKey != "github.com/acme/api" {
		t.Fatalf("conflict details = %+v, want sorted keys and acme winner", details)
	}
}

func TestApplyEventsQuarantinesDivergentDuplicateEventID(t *testing.T) {
	ctx := context.Background()
	st, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(ctx, "test", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	device, err := st.EnsureDevice(ctx, "device-a")
	if err != nil {
		t.Fatal(err)
	}
	event, err := NewProjectEvent(device.ID, EventProjectAdded, 1, ProjectPayload{
		Path:          "work/acme/api",
		Type:          "git_repo",
		RemoteURL:     "git@github.com:acme/api.git",
		RemoteKey:     "github.com/acme/api",
		DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEvents(ctx, st, []state.Event{event}); err != nil {
		t.Fatal(err)
	}
	divergent := event
	divergent.PayloadJSON = `{"path":"work/acme/other","type":"git_repo","remote_url":"git@github.com:acme/other.git","remote_key":"github.com/acme/other","default_branch":"main"}`
	divergent.ContentHash = state.ContentHash(divergent.PayloadJSON)
	if _, err := ApplyEvents(ctx, st, []state.Event{divergent}); err != nil {
		t.Fatalf("ApplyEvents divergent duplicate should quarantine and continue: %v", err)
	}
	conflicts, err := st.OpenConflicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !hasConflictType(conflicts, ConflictEventVerification) {
		t.Fatalf("conflicts = %+v, want %s", conflicts, ConflictEventVerification)
	}
}

func TestApplyEventsRejectsBrokenPrevEventHashAndRecordsConflict(t *testing.T) {
	ctx := context.Background()
	st, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(ctx, "test", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	device, err := st.EnsureDevice(ctx, "device-a")
	if err != nil {
		t.Fatal(err)
	}
	first, err := NewProjectEvent(device.ID, EventProjectAdded, 10, ProjectPayload{
		Path:          "work/acme/api",
		Type:          "git_repo",
		RemoteURL:     "git@github.com:acme/api.git",
		RemoteKey:     "github.com/acme/api",
		DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEvents(ctx, st, []state.Event{first}); err != nil {
		t.Fatal(err)
	}
	second, err := NewProjectEvent(device.ID, EventProjectAdded, 20, ProjectPayload{
		Path:          "work/acme/web",
		Type:          "git_repo",
		RemoteURL:     "git@github.com:acme/web.git",
		RemoteKey:     "github.com/acme/web",
		DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	second.PrevEventHash = "sha256:broken"
	_, err = ApplyEvents(ctx, st, []state.Event{second})
	// SYNC-05/CODE-01: a hash-chain break records a conflict and continues
	// (does not abort the batch), so ApplyEvents returns nil.
	if err != nil {
		t.Fatalf("ApplyEvents broken chain error = %v, want nil (conflict recorded, batch continues)", err)
	}
	conflicts, err := st.OpenConflicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 || conflicts[0].Type != "event_hash_chain_break" {
		t.Fatalf("conflicts = %+v, want one event_hash_chain_break", conflicts)
	}
	projects, err := st.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].Path != "work/acme/api" {
		t.Fatalf("projects after broken chain = %+v, want only first project", projects)
	}
	second.PrevEventHash = first.ContentHash
	if _, err := ApplyEvents(ctx, st, []state.Event{second}); err != nil {
		t.Fatal(err)
	}
	projects, err = st.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 2 {
		t.Fatalf("projects after valid chain = %+v, want two projects", projects)
	}
}

func TestApplyEventsHonorsProjectDeleteTombstoneHLC(t *testing.T) {
	ctx := context.Background()
	st, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(ctx, "test", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	device, err := st.EnsureDevice(ctx, "device-a")
	if err != nil {
		t.Fatal(err)
	}
	add, err := NewProjectEvent(device.ID, EventProjectAdded, 10, ProjectPayload{
		Path:          "work/acme/api",
		Type:          "git_repo",
		RemoteURL:     "git@github.com:acme/api.git",
		RemoteKey:     "github.com/acme/api",
		DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	del, err := NewProjectEvent(device.ID, EventProjectDeleted, 20, ProjectPayload{Path: "work/acme/api"})
	if err != nil {
		t.Fatal(err)
	}
	olderRestore, err := NewProjectEvent(device.ID, EventProjectAdded, 15, ProjectPayload{
		Path:          "work/acme/api",
		Type:          "git_repo",
		RemoteURL:     "git@github.com:acme/api.git",
		RemoteKey:     "github.com/acme/api",
		DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	newerRestore, err := NewProjectEvent(device.ID, EventProjectAdded, 25, ProjectPayload{
		Path:          "work/acme/api",
		Type:          "git_repo",
		RemoteURL:     "git@github.com:acme/api.git",
		RemoteKey:     "github.com/acme/api",
		DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEvents(ctx, st, []state.Event{add, del, olderRestore}); err != nil {
		t.Fatal(err)
	}
	projects, err := st.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 0 {
		t.Fatalf("projects after delete+older restore = %+v, want none", projects)
	}
	if _, err := ApplyEvents(ctx, st, []state.Event{newerRestore}); err != nil {
		t.Fatal(err)
	}
	projects, err = st.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].Path != "work/acme/api" {
		t.Fatalf("projects after newer restore = %+v, want restored project", projects)
	}
}

func TestCreateProjectEventUsesPersistedLocalClock(t *testing.T) {
	ctx := context.Background()
	st, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(ctx, "test", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	device, err := st.EnsureDevice(ctx, "device-a")
	if err != nil {
		t.Fatal(err)
	}
	event, err := CreateProjectEvent(ctx, st, EventProjectAdded, ProjectPayload{
		Path:          "work/acme/api",
		Type:          "git_repo",
		RemoteURL:     "git@github.com:acme/api.git",
		RemoteKey:     "github.com/acme/api",
		DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if event.DeviceID != device.ID || event.Seq != 1 || event.HLC == 0 {
		t.Fatalf("local project event = %+v, want device, seq=1, nonzero hlc", event)
	}
	pending, err := st.PendingEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != event.ID {
		t.Fatalf("pending events = %+v, want created event", pending)
	}
}
