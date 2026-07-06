package sync

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/state"
)

// h scales a small integer into a plausible HLC (physical<<logical) so imported
// coordinates sort and compare the same way applied event HLCs do.
func h(n int64) int64 { return n << hlcLogicalBits }

func gitEntry(path, remoteKey string, hlc int64, dev, evt string) SnapshotEntry {
	return SnapshotEntry{
		Path:                path,
		PathKey:             path,
		Type:                "git_repo",
		Status:              "active",
		SourceEventHLC:      hlc,
		SourceEventDeviceID: dev,
		SourceEventID:       evt,
		Git: &SnapshotGit{
			RemoteURL:     "git@github.com:acme/" + remoteKey + ".git",
			RemoteKey:     "github.com/acme/" + remoteKey,
			DefaultBranch: "main",
		},
	}
}

func importOnly(entries []SnapshotEntry, tombstones []SnapshotTombstone) Snapshot {
	return Snapshot{
		WorkspaceID: "ws_test",
		ProducedBy:  "dev_a",
		HLC:         h(1000),
		Floor:       Cursor{"dev_a": 5},
		Entries:     entries,
		Tombstones:  tombstones,
	}
}

func remoteKeyByPath(t *testing.T, st *state.Store, path string) string {
	t.Helper()
	p, err := st.ProjectByPath(context.Background(), path)
	if err != nil {
		t.Fatalf("ProjectByPath(%s): %v", path, err)
	}
	return p.RemoteKey
}

// TestImportSnapshotLWWNewAndOverwrite: a snapshot entry for an unknown path is
// materialized; a NEWER coordinate overwrites; an OLDER coordinate is a no-op.
func TestImportSnapshotLWWMatrix(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)

	// New path.
	if err := ImportSnapshot(ctx, st, importOnly([]SnapshotEntry{
		gitEntry("work/api", "api", h(500), "dev_a", "evt_500"),
	}, nil), "sha1", "hub1"); err != nil {
		t.Fatal(err)
	}
	if got := remoteKeyByPath(t, st, "work/api"); got != "github.com/acme/api" {
		t.Fatalf("after new import remote_key = %q", got)
	}

	// Older snapshot coordinate: must NOT overwrite.
	if err := ImportSnapshot(ctx, st, importOnly([]SnapshotEntry{
		gitEntry("work/api", "stale", h(300), "dev_a", "evt_300"),
	}, nil), "sha2", "hub1"); err != nil {
		t.Fatal(err)
	}
	if got := remoteKeyByPath(t, st, "work/api"); got != "github.com/acme/api" {
		t.Fatalf("older import overwrote remote_key = %q, want unchanged", got)
	}

	// Newer snapshot coordinate: must overwrite.
	if err := ImportSnapshot(ctx, st, importOnly([]SnapshotEntry{
		gitEntry("work/api", "fresh", h(900), "dev_a", "evt_900"),
	}, nil), "sha3", "hub1"); err != nil {
		t.Fatal(err)
	}
	if got := remoteKeyByPath(t, st, "work/api"); got != "github.com/acme/fresh" {
		t.Fatalf("newer import remote_key = %q, want github.com/acme/fresh", got)
	}
}

// TestImportSnapshotTombstoneGating: a tombstone deletes a path whose local add
// is older, but is skipped when the local add is newer.
func TestImportSnapshotTombstoneGating(t *testing.T) {
	ctx := context.Background()

	t.Run("older add is deleted", func(t *testing.T) {
		st, device := newSyncStore(t)
		add, err := NewProjectEvent(device.ID, EventProjectAdded, h(300), ProjectPayload{
			Path: "work/gone", Type: "git_repo", RemoteKey: "github.com/acme/gone",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ApplyEvents(ctx, st, []state.Event{add}); err != nil {
			t.Fatal(err)
		}
		snap := importOnly(nil, []SnapshotTombstone{{PathKey: "work/gone", TombstoneHLC: h(500), SourceEventDeviceID: "dev_a", SourceEventID: "evt_del"}})
		if err := ImportSnapshot(ctx, st, snap, "sha", "hub1"); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ProjectByPath(ctx, "work/gone"); err == nil {
			t.Fatal("work/gone still active after a newer tombstone")
		}
	})

	t.Run("newer add survives", func(t *testing.T) {
		st, device := newSyncStore(t)
		add, err := NewProjectEvent(device.ID, EventProjectAdded, h(900), ProjectPayload{
			Path: "work/keep", Type: "git_repo", RemoteKey: "github.com/acme/keep",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ApplyEvents(ctx, st, []state.Event{add}); err != nil {
			t.Fatal(err)
		}
		snap := importOnly(nil, []SnapshotTombstone{{PathKey: "work/keep", TombstoneHLC: h(500), SourceEventDeviceID: "dev_a", SourceEventID: "evt_del"}})
		if err := ImportSnapshot(ctx, st, snap, "sha", "hub1"); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ProjectByPath(ctx, "work/keep"); err != nil {
			t.Fatalf("work/keep must survive an older tombstone: %v", err)
		}
	})

	t.Run("tombstone of an unknown path blocks a later stale add", func(t *testing.T) {
		st, _ := newSyncStore(t)
		snap := importOnly(nil, []SnapshotTombstone{{PathKey: "work/never", TombstoneHLC: h(500)}})
		if err := ImportSnapshot(ctx, st, snap, "sha", "hub1"); err != nil {
			t.Fatal(err)
		}
		// An add older than the tombstone must be suppressed on import.
		entrySnap := importOnly([]SnapshotEntry{gitEntry("work/never", "never", h(400), "dev_a", "evt_400")}, nil)
		if err := ImportSnapshot(ctx, st, entrySnap, "sha2", "hub1"); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ProjectByPath(ctx, "work/never"); err == nil {
			t.Fatal("a stale add resurrected a tombstoned path")
		}
	})
}

// TestImportSnapshotDirtyTombstoneConflict: a snapshot delete must not destroy a
// dirty local checkout; it raises a pending_delete_conflict and leaves the
// project active (mirrors the event apply path).
func TestImportSnapshotDirtyTombstoneConflict(t *testing.T) {
	ctx := context.Background()
	st, device := newSyncStore(t)
	add, err := NewProjectEvent(device.ID, EventProjectAdded, h(300), ProjectPayload{
		Path: "work/dirty", Type: "git_repo", RemoteKey: "github.com/acme/dirty",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEvents(ctx, st, []state.Event{add}); err != nil {
		t.Fatal(err)
	}
	project, err := st.ProjectByPath(ctx, "work/dirty")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateProjectLocalState(ctx, project.ID, "/tmp/Code/work/dirty", "available", "dirty"); err != nil {
		t.Fatal(err)
	}
	snap := importOnly(nil, []SnapshotTombstone{{PathKey: "work/dirty", TombstoneHLC: h(900), SourceEventDeviceID: "dev_a", SourceEventID: "evt_del"}})
	if err := ImportSnapshot(ctx, st, snap, "sha", "hub1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ProjectByPath(ctx, "work/dirty"); err != nil {
		t.Fatalf("dirty project must survive a snapshot delete: %v", err)
	}
	conflicts, err := st.OpenConflicts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !hasConflictType(conflicts, ConflictPendingDelete) {
		t.Fatalf("conflicts = %+v, want %s", conflicts, ConflictPendingDelete)
	}
}

// TestImportSnapshotDraftPointerIdempotent: a snapshot draft pointer is recorded
// once and re-imports are no-ops.
func TestImportSnapshotDraftPointerIdempotent(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	ref := "age_blob:" + strings.Repeat("a", 64)
	entry := SnapshotEntry{
		Path:                "notes",
		PathKey:             "notes",
		Type:                "draft_project",
		Status:              "active",
		SourceEventHLC:      h(400),
		SourceEventDeviceID: "dev_a",
		SourceEventID:       "evt_draft_entry",
		Draft: &SnapshotDraft{
			BlobRef:             ref,
			ByteSize:            10,
			FileCount:           2,
			SourceEventHLC:      h(410),
			SourceEventDeviceID: "dev_a",
			SourceEventID:       "evt_draft_snap",
		},
	}
	snap := importOnly([]SnapshotEntry{entry}, nil)
	if err := ImportSnapshot(ctx, st, snap, "sha", "hub1"); err != nil {
		t.Fatal(err)
	}
	if err := ImportSnapshot(ctx, st, snap, "sha", "hub1"); err != nil {
		t.Fatal(err)
	}
	refs, err := st.AllBlobRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, r := range refs {
		if r == ref {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("draft blob ref appears %d times, want exactly 1 (idempotent): %+v", count, refs)
	}
}

// TestImportSnapshotReImportIdempotent: importing the same snapshot twice leaves
// identical state and does not error.
func TestImportSnapshotReImportIdempotent(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	snap := importOnly([]SnapshotEntry{
		gitEntry("work/api", "api", h(500), "dev_a", "evt_500"),
	}, []SnapshotTombstone{{PathKey: "work/old", TombstoneHLC: h(400)}})
	if err := ImportSnapshot(ctx, st, snap, "sha", "hub1"); err != nil {
		t.Fatal(err)
	}
	if err := ImportSnapshot(ctx, st, snap, "sha", "hub1"); err != nil {
		t.Fatalf("re-import: %v", err)
	}
	projects, err := st.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].Path != "work/api" {
		t.Fatalf("projects after re-import = %+v, want only work/api", projects)
	}
}

// TestImportThenApplyEqualsApplyThenImport: for one path with the SAME remote
// (pure LWW on both sides — a snapshot entry already embeds the reconcile winner
// for a same-path/different-remote conflict, so this is the realistic case), the
// merge converges regardless of whether the snapshot import or the event apply
// runs first. The higher-HLC writer wins in both orders.
func TestImportThenApplyEqualsApplyThenImport(t *testing.T) {
	ctx := context.Background()
	const remote = "github.com/acme/p"

	sourceHLC := func(t *testing.T, st *state.Store) int64 {
		t.Helper()
		p, err := st.ProjectByPath(ctx, "work/p")
		if err != nil {
			t.Fatal(err)
		}
		return p.SourceEventHLC
	}
	// gitEntry builds a "github.com/acme/<key>" remote; use "p" so both writers
	// share the remote and take the plain-LWW path (no reconcile).
	entry := SnapshotEntry{
		Path: "work/p", PathKey: "work/p", Type: "git_repo", Status: "active",
		SourceEventHLC: h(300), SourceEventDeviceID: "dev_x", SourceEventID: "evt_300",
		Git: &SnapshotGit{RemoteURL: "git@github.com:acme/p.git", RemoteKey: remote, DefaultBranch: "main"},
	}

	// import (older, hlc 300) then apply event (newer, hlc 600) → event wins.
	stA, devA := newSyncStore(t)
	if err := ImportSnapshot(ctx, stA, importOnly([]SnapshotEntry{entry}, nil), "sha", "hub1"); err != nil {
		t.Fatal(err)
	}
	addA, err := NewProjectEvent(devA.ID, EventProjectAdded, h(600), ProjectPayload{
		Path: "work/p", Type: "git_repo", RemoteKey: remote,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEvents(ctx, stA, []state.Event{addA}); err != nil {
		t.Fatal(err)
	}

	// apply event (newer, hlc 600) then import (older, hlc 300) → event still wins.
	stB, devB := newSyncStore(t)
	addB, err := NewProjectEvent(devB.ID, EventProjectAdded, h(600), ProjectPayload{
		Path: "work/p", Type: "git_repo", RemoteKey: remote,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEvents(ctx, stB, []state.Event{addB}); err != nil {
		t.Fatal(err)
	}
	if err := ImportSnapshot(ctx, stB, importOnly([]SnapshotEntry{entry}, nil), "sha", "hub1"); err != nil {
		t.Fatal(err)
	}

	if a, b := sourceHLC(t, stA), sourceHLC(t, stB); a != b || a != h(600) {
		t.Fatalf("convergence failed: import-then-apply source_hlc=%d apply-then-import source_hlc=%d, want %d", a, b, h(600))
	}
}

func envPointer(hlc int64, dev, evt string) *SnapshotEnv {
	return &SnapshotEnv{
		Name:                "default",
		Provider:            "devstrap_encrypted",
		Mode:                "hydrate_or_runtime",
		BlobRef:             "age_blob:" + evt,
		VarNames:            []string{"API_TOKEN"},
		SourceEventHLC:      hlc,
		SourceEventDeviceID: dev,
		SourceEventID:       evt,
	}
}

// TestSnapshotRoundTripsEnvProfile: an env profile captured via the event plane
// survives BuildSnapshot → ImportSnapshot into a fresh store (ENV-SYNC-01 /
// P4-HUB-11: profiles must outlive event-log compaction), and re-import is
// idempotent.
func TestSnapshotRoundTripsEnvProfile(t *testing.T) {
	ctx := context.Background()
	stA, deviceA := newSyncStore(t)
	signing := addRemoteDeviceForApplyTest(t, stA, "device-env", "approved")
	now := time.Now().UnixMilli()
	add := projEvent(t, deviceA.ID, EventProjectAdded, now, "work/acme/api", "github.com/acme/api")
	env := signedEnvProfileEvent(t, signing, "evt_env", "device-env", 1, now+1, EnvProfilePayload{
		Path:     "work/acme/api",
		Profile:  "default",
		Provider: "devstrap_encrypted",
		Mode:     "hydrate_or_runtime",
		BlobRef:  "age_blob:roundtrip",
		VarNames: []string{"API_TOKEN", "DB_URL"},
	})
	if _, err := ApplyEvents(ctx, stA, []state.Event{add, env}); err != nil {
		t.Fatal(err)
	}
	snap, err := BuildSnapshot(ctx, stA, deviceA.ID, h(1000), Cursor{deviceA.ID: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Entries) != 1 || snap.Entries[0].Env == nil {
		t.Fatalf("snapshot must carry the env pointer: %#v", snap.Entries)
	}
	if got := snap.Entries[0].Env.BlobRef; got != "age_blob:roundtrip" {
		t.Fatalf("env blob ref = %q", got)
	}

	stB, _ := newSyncStore(t)
	if err := ImportSnapshot(ctx, stB, snap, "sha-env", "hub-env"); err != nil {
		t.Fatal(err)
	}
	project, err := stB.ProjectByPath(ctx, "work/acme/api")
	if err != nil {
		t.Fatal(err)
	}
	profile, bindings, err := stB.EnvProfileForProject(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Provider != "devstrap_encrypted" || len(bindings) != 2 || bindings[0].EncryptedValueRef != "age_blob:roundtrip" {
		t.Fatalf("profile=%#v bindings=%#v", profile, bindings)
	}
	// Re-import: idempotent (equal coordinates do not win the LWW compare).
	if err := ImportSnapshot(ctx, stB, snap, "sha-env", "hub-env"); err != nil {
		t.Fatal(err)
	}
	if _, bindings, err = stB.EnvProfileForProject(ctx, project.ID); err != nil || len(bindings) != 2 {
		t.Fatalf("re-import changed bindings: %v %#v", err, bindings)
	}
}

// TestImportSnapshotEnvLWW: the env pointer merges by its OWN coordinate — an
// older pointer never regresses a newer local profile, a newer pointer wins
// even when the entry row itself LOSES the project LWW compare.
func TestImportSnapshotEnvLWW(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)

	// Local state: project at h(800) with an env profile at h(900).
	if err := ImportSnapshot(ctx, st, importOnly([]SnapshotEntry{
		gitEntry("work/api", "api", h(800), "dev_local", "evt_800"),
	}, nil), "sha-env", "hub-env"); err != nil {
		t.Fatal(err)
	}
	project, err := st.ProjectByPath(ctx, "work/api")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WithTx(ctx, func(tx *state.Tx) error {
		_, err := tx.UpsertEnvProfileTx(ctx, project.ID, state.EnvProfileParams{
			Name:     "default",
			Provider: "devstrap_encrypted",
			Mode:     "hydrate_or_runtime",
			BlobRef:  "age_blob:local900",
			VarNames: []string{"API_TOKEN"},
		}, state.Event{ID: "evt_900", DeviceID: "dev_local", HLC: h(900)})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	assertEnvRef := func(want string) {
		t.Helper()
		_, bindings, err := st.EnvProfileForProject(ctx, project.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(bindings) == 0 || bindings[0].EncryptedValueRef != want {
			t.Fatalf("env ref = %#v, want %s", bindings, want)
		}
	}

	// Older env pointer on a WINNING entry (project h(850) loses to nothing —
	// entry at h(850) beats h(800)) must not regress the newer local profile.
	older := gitEntry("work/api", "api", h(850), "dev_a", "evt_850")
	older.Env = envPointer(h(700), "dev_a", "evt_700")
	if err := ImportSnapshot(ctx, st, importOnly([]SnapshotEntry{older}, nil), "sha-env2", "hub-env"); err != nil {
		t.Fatal(err)
	}
	assertEnvRef("age_blob:local900")

	// Newer env pointer on a LOSING entry (project h(820) < local h(850) now)
	// must still win the independent env compare.
	newer := gitEntry("work/api", "api", h(820), "dev_b", "evt_820")
	newer.Env = envPointer(h(950), "dev_b", "evt_950")
	if err := ImportSnapshot(ctx, st, importOnly([]SnapshotEntry{newer}, nil), "sha-env3", "hub-env"); err != nil {
		t.Fatal(err)
	}
	assertEnvRef("age_blob:evt_950")
}
