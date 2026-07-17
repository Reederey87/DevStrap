package sync

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/state"
)

func gitstateEvent(t *testing.T, id, dev string, seq, hlc int64, payload GitstatePayload) state.Event {
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
		Type:        EventGitstateObserved,
		PayloadJSON: string(raw),
		ContentHash: state.ContentHash(string(raw)),
	}
}

func signedGitstateEvent(t *testing.T, signing devicekeys.SigningIdentity, id, dev string, seq, hlc int64, payload GitstatePayload) state.Event {
	t.Helper()
	ev := gitstateEvent(t, id, dev, seq, hlc, payload)
	sig, err := devicekeys.Sign(signing.Private, "devstrap:event:v2", state.EventSignaturePayloadV2(ev))
	if err != nil {
		t.Fatal(err)
	}
	ev.DeviceSig = sig
	return ev
}

// TestApplyGitstateEventMirrorsWithoutRequiringProjectToExist: unlike
// env.profile.updated/draft.snapshot.created, a gitstate observation for a
// project this device has never heard of must still apply — there is no
// pending-project quarantine class for this event type (migration 00029).
func TestApplyGitstateEventMirrorsWithoutRequiringProjectToExist(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	signing := addRemoteDeviceForApplyTest(t, st, "device-gs", "approved")
	now := time.Now().UnixMilli()
	ev := signedGitstateEvent(t, signing, "evt_gs1", "device-gs", 1, now, GitstatePayload{
		Path: "work/acme/unknown-project", Branch: "main", HeadSHA: "abc123",
		UpstreamBranch: "origin/main", UpstreamSHA: "def456",
		DirtyCount: 2, UntrackedCount: 1, AheadCount: 3,
	})
	_, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{ev}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Quarantined != 0 {
		t.Fatalf("stats=%+v, want the gitstate event applied without a project", stats)
	}
	rows, err := st.DeviceGitstateForProject(ctx, "work/acme/unknown-project")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows=%#v, want exactly one mirrored row", rows)
	}
	got := rows[0]
	if got.DeviceID != "device-gs" || got.Branch != "main" || got.HeadSHA != "abc123" ||
		got.UpstreamBranch != "origin/main" || got.UpstreamSHA != "def456" ||
		got.DirtyCount != 2 || got.UntrackedCount != 1 || got.AheadCount != 3 {
		t.Fatalf("mirrored row = %+v, unexpected fields", got)
	}
	if got.ObservedAtHLC != ev.HLC || got.SourceEventID != ev.ID {
		t.Fatalf("mirrored row = %+v, want observed_at_hlc=%d source_event_id=%s", got, ev.HLC, ev.ID)
	}
}

// TestApplyGitstateEventOverwritesPreviousObservation: apply is MIRROR-ONLY —
// a later observation from the same device for the same project replaces the
// row instead of appending a history entry.
func TestApplyGitstateEventOverwritesPreviousObservation(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	signing := addRemoteDeviceForApplyTest(t, st, "device-gs", "approved")
	now := time.Now().UnixMilli()
	first := signedGitstateEvent(t, signing, "evt_gs_first", "device-gs", 1, now, GitstatePayload{
		Path: "work/acme/proj", Branch: "main", HeadSHA: "aaa", DirtyCount: 1,
	})
	second := signedGitstateEvent(t, signing, "evt_gs_second", "device-gs", 2, now+1, GitstatePayload{
		Path: "work/acme/proj", Branch: "main", HeadSHA: "bbb", DirtyCount: 0, StashCount: 1,
	})
	if _, err := ApplyEvents(ctx, st, []state.Event{first, second}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.DeviceGitstateForProject(ctx, "work/acme/proj")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows=%#v, want exactly one mirrored row (overwrite, not append)", rows)
	}
	if rows[0].HeadSHA != "bbb" || rows[0].DirtyCount != 0 || rows[0].StashCount != 1 {
		t.Fatalf("mirrored row = %+v, want the latest observation to win", rows[0])
	}
}

// TestApplyGitstateEventOutOfOrderRedeliveryDoesNotRegressMirror: a
// re-delivery of an OLDER observation (lower HLC) after a newer one has
// already applied must not roll the mirror back.
func TestApplyGitstateEventOutOfOrderRedeliveryDoesNotRegressMirror(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	signing := addRemoteDeviceForApplyTest(t, st, "device-gs", "approved")
	now := time.Now().UnixMilli()
	older := signedGitstateEvent(t, signing, "evt_gs_older", "device-gs", 1, now, GitstatePayload{
		Path: "work/acme/proj", Branch: "main", HeadSHA: "aaa", DirtyCount: 5,
	})
	newer := signedGitstateEvent(t, signing, "evt_gs_newer", "device-gs", 2, now+1, GitstatePayload{
		Path: "work/acme/proj", Branch: "main", HeadSHA: "bbb", DirtyCount: 0,
	})
	if _, err := ApplyEvents(ctx, st, []state.Event{newer}); err != nil {
		t.Fatal(err)
	}
	// Re-deliver the older event directly through the apply path (bypassing
	// InsertEvent's own de-dup) to exercise the mirror's own HLC guard.
	if _, err := ApplyEvents(ctx, st, []state.Event{older}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.DeviceGitstateForProject(ctx, "work/acme/proj")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].HeadSHA != "bbb" || rows[0].DirtyCount != 0 {
		t.Fatalf("mirrored row = %#v, want the newer observation to remain after an out-of-order redelivery", rows)
	}
}

// TestApplyGitstateEventTracksMultipleDevicesIndependently: two devices
// observing the same project are two independent rows keyed by device_id —
// this is a cross-machine visibility plane, not a single project-level flag.
func TestApplyGitstateEventTracksMultipleDevicesIndependently(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	signingA := addRemoteDeviceForApplyTest(t, st, "device-a", "approved")
	signingB := addRemoteDeviceForApplyTest(t, st, "device-b", "approved")
	now := time.Now().UnixMilli()
	evA := signedGitstateEvent(t, signingA, "evt_gs_a", "device-a", 1, now, GitstatePayload{
		Path: "work/acme/proj", Branch: "main", HeadSHA: "aaa",
	})
	evB := signedGitstateEvent(t, signingB, "evt_gs_b", "device-b", 1, now+1, GitstatePayload{
		Path: "work/acme/proj", Branch: "feature", HeadSHA: "bbb", DirtyCount: 4,
	})
	if _, err := ApplyEvents(ctx, st, []state.Event{evA, evB}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.DeviceGitstateForProject(ctx, "work/acme/proj")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows=%#v, want one mirrored row per device", rows)
	}
}

// TestApplyGitstateEventMalformedPayloadQuarantinesWithoutAbort mirrors the
// env/draft malformed-payload convention (#133): only an approved signer
// reaches the apply handler, so a payload that can never decode must
// quarantine as consumed instead of aborting the pull batch.
func TestApplyGitstateEventMalformedPayloadQuarantinesWithoutAbort(t *testing.T) {
	ctx := context.Background()
	st, device := newSyncStore(t)
	signing := addRemoteDeviceForApplyTest(t, st, "device-gs", "approved")
	now := time.Now().UnixMilli()
	bad := state.Event{
		ID:          "evt_bad_gs",
		DeviceID:    "device-gs",
		Seq:         1,
		HLC:         now << hlcLogicalBits,
		Type:        EventGitstateObserved,
		PayloadJSON: `{"path":`,
	}
	bad.ContentHash = state.ContentHash(bad.PayloadJSON)
	sig, err := devicekeys.Sign(signing.Private, "devstrap:event:v2", state.EventSignaturePayloadV2(bad))
	if err != nil {
		t.Fatal(err)
	}
	bad.DeviceSig = sig
	good := projEvent(t, device.ID, EventProjectAdded, now+1, "work/acme/after-gitstate", "github.com/acme/after-gitstate")
	good.Seq = 1
	_, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{bad, good}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Quarantined != 1 || stats.CursorHeld {
		t.Fatalf("stats=%+v, want malformed gitstate quarantined as consumed", stats)
	}
	if _, err := st.ProjectByPath(ctx, "work/acme/after-gitstate"); err != nil {
		t.Fatalf("batch must continue past malformed gitstate event: %v", err)
	}
	conflicts, err := st.OpenConflictsByType(ctx, ConflictEventVerification)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range conflicts {
		var d eventVerificationConflictDetails
		if json.Unmarshal([]byte(c.DetailsJSON), &d) == nil && d.EventID == "evt_bad_gs" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want a verification-failure conflict for evt_bad_gs, got %#v", conflicts)
	}
}

// TestApplyGitstateEventUnsafePathQuarantinesWithoutAbort: a verified event
// whose path can never resolve (path escape) must quarantine as consumed like
// the draft/env unsafe-path case, not abort the batch.
func TestApplyGitstateEventUnsafePathQuarantinesWithoutAbort(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	signing := addRemoteDeviceForApplyTest(t, st, "device-gs", "approved")
	now := time.Now().UnixMilli()
	ev := signedGitstateEvent(t, signing, "evt_gs_escape", "device-gs", 1, now, GitstatePayload{
		Path: "../escape", Branch: "main", HeadSHA: "aaa",
	})
	_, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{ev}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Quarantined != 1 || stats.CursorHeld {
		t.Fatalf("stats=%+v, want the unsafe-path gitstate event quarantined as consumed", stats)
	}
}

// TestNewGitstateEventStampsTypeAndContentHash pins the unsigned-constructor
// contract (mirrors NewDraftSnapshotEvent/NewEnvProfileEvent): InsertLocalEvent
// stamps HLC/seq/device id/signature, so the constructor itself only sets type,
// payload, and content hash.
func TestNewGitstateEventStampsTypeAndContentHash(t *testing.T) {
	raw, err := json.Marshal(GitstatePayload{Path: "work/acme/proj", Branch: "main", HeadSHA: "aaa"})
	if err != nil {
		t.Fatal(err)
	}
	ev := NewGitstateEvent(string(raw))
	if ev.Type != EventGitstateObserved {
		t.Fatalf("Type = %q, want %q", ev.Type, EventGitstateObserved)
	}
	if ev.PayloadJSON != string(raw) {
		t.Fatalf("PayloadJSON = %q, want %q", ev.PayloadJSON, string(raw))
	}
	if ev.ContentHash != state.ContentHash(string(raw)) {
		t.Fatalf("ContentHash = %q, want the payload's content hash", ev.ContentHash)
	}
}
