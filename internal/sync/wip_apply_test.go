package sync

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/state"
)

func wipEvent(t *testing.T, id, dev string, seq, hlc int64, payload WipPayload) state.Event {
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
		Type:        EventRepoWipPushed,
		PayloadJSON: string(raw),
		ContentHash: state.ContentHash(string(raw)),
	}
}

func signedWipEvent(t *testing.T, signing devicekeys.SigningIdentity, id, dev string, seq, hlc int64, payload WipPayload) state.Event {
	t.Helper()
	ev := wipEvent(t, id, dev, seq, hlc, payload)
	sig, err := devicekeys.Sign(signing.Private, "devstrap:event:v2", state.EventSignaturePayloadV2(ev))
	if err != nil {
		t.Fatal(err)
	}
	ev.DeviceSig = sig
	return ev
}

// TestApplyWipEventMirrorsWithoutRequiringProjectToExist mirrors
// TestApplyGitstateEventMirrorsWithoutRequiringProjectToExist: unlike
// env.profile.updated/draft.snapshot.created, a WIP push observation for a
// project this device has never heard of must still apply — there is no
// pending-project quarantine class for this event type (migration 00030).
func TestApplyWipEventMirrorsWithoutRequiringProjectToExist(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	signing := addRemoteDeviceForApplyTest(t, st, "device-wip", "approved")
	now := time.Now().UnixMilli()
	ev := signedWipEvent(t, signing, "evt_wip1", "device-wip", 1, now, WipPayload{
		Path: "work/acme/unknown-project", Ref: "refs/devstrap/wip/device-wip/work/acme/unknown-project",
		SHA: "abc123", BaseSHA: "def456", CapturedAt: "2026-07-17T00:00:00Z",
	})
	_, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{ev}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Quarantined != 0 {
		t.Fatalf("stats=%+v, want the wip event applied without a project", stats)
	}
	rows, err := st.DeviceWipForProject(ctx, "work/acme/unknown-project")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows=%#v, want exactly one mirrored row", rows)
	}
	got := rows[0]
	if got.DeviceID != "device-wip" || got.Ref != "refs/devstrap/wip/device-wip/work/acme/unknown-project" ||
		got.SHA != "abc123" || got.BaseSHA != "def456" || got.CapturedAt != "2026-07-17T00:00:00Z" {
		t.Fatalf("mirrored row = %+v, unexpected fields", got)
	}
	if got.ObservedAtHLC != ev.HLC || got.SourceEventID != ev.ID {
		t.Fatalf("mirrored row = %+v, want observed_at_hlc=%d source_event_id=%s", got, ev.HLC, ev.ID)
	}
}

// TestApplyWipEventOverwritesPreviousObservation mirrors
// TestApplyGitstateEventOverwritesPreviousObservation: apply is MIRROR-ONLY —
// a later push from the same device for the same project replaces the row
// instead of appending a history entry.
func TestApplyWipEventOverwritesPreviousObservation(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	signing := addRemoteDeviceForApplyTest(t, st, "device-wip", "approved")
	now := time.Now().UnixMilli()
	first := signedWipEvent(t, signing, "evt_wip_first", "device-wip", 1, now, WipPayload{
		Path: "work/acme/proj", Ref: "refs/devstrap/wip/device-wip/work/acme/proj", SHA: "aaa", BaseSHA: "base1",
	})
	second := signedWipEvent(t, signing, "evt_wip_second", "device-wip", 2, now+1, WipPayload{
		Path: "work/acme/proj", Ref: "refs/devstrap/wip/device-wip/work/acme/proj", SHA: "bbb", BaseSHA: "base2",
	})
	if _, err := ApplyEvents(ctx, st, []state.Event{first, second}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.DeviceWipForProject(ctx, "work/acme/proj")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows=%#v, want exactly one mirrored row (overwrite, not append)", rows)
	}
	if rows[0].SHA != "bbb" || rows[0].BaseSHA != "base2" {
		t.Fatalf("mirrored row = %+v, want the latest observation to win", rows[0])
	}
}

// TestApplyWipEventOutOfOrderRedeliveryDoesNotRegressMirror mirrors
// TestApplyGitstateEventOutOfOrderRedeliveryDoesNotRegressMirror: a
// re-delivery of an OLDER observation (lower HLC) after a newer one has
// already applied must not roll the mirror back.
func TestApplyWipEventOutOfOrderRedeliveryDoesNotRegressMirror(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	signing := addRemoteDeviceForApplyTest(t, st, "device-wip", "approved")
	now := time.Now().UnixMilli()
	older := signedWipEvent(t, signing, "evt_wip_older", "device-wip", 1, now, WipPayload{
		Path: "work/acme/proj", SHA: "aaa",
	})
	newer := signedWipEvent(t, signing, "evt_wip_newer", "device-wip", 2, now+1, WipPayload{
		Path: "work/acme/proj", SHA: "bbb",
	})
	if _, err := ApplyEvents(ctx, st, []state.Event{newer}); err != nil {
		t.Fatal(err)
	}
	// Re-deliver the older event directly through the apply path (bypassing
	// InsertEvent's own de-dup) to exercise the mirror's own HLC guard.
	if _, err := ApplyEvents(ctx, st, []state.Event{older}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.DeviceWipForProject(ctx, "work/acme/proj")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].SHA != "bbb" {
		t.Fatalf("mirrored row = %#v, want the newer observation to remain after an out-of-order redelivery", rows)
	}
}

// TestApplyWipEventTracksMultipleDevicesIndependently mirrors
// TestApplyGitstateEventTracksMultipleDevicesIndependently: two devices
// pushing WIP for the same project are two independent rows keyed by
// device_id — this is a cross-machine visibility plane, not a single
// project-level flag.
func TestApplyWipEventTracksMultipleDevicesIndependently(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	signingA := addRemoteDeviceForApplyTest(t, st, "device-a", "approved")
	signingB := addRemoteDeviceForApplyTest(t, st, "device-b", "approved")
	now := time.Now().UnixMilli()
	evA := signedWipEvent(t, signingA, "evt_wip_a", "device-a", 1, now, WipPayload{
		Path: "work/acme/proj", SHA: "aaa",
	})
	evB := signedWipEvent(t, signingB, "evt_wip_b", "device-b", 1, now+1, WipPayload{
		Path: "work/acme/proj", SHA: "bbb",
	})
	if _, err := ApplyEvents(ctx, st, []state.Event{evA, evB}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.DeviceWipForProject(ctx, "work/acme/proj")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows=%#v, want one mirrored row per device", rows)
	}
}

// TestApplyWipEventMalformedPayloadQuarantinesWithoutAbort mirrors
// TestApplyGitstateEventMalformedPayloadQuarantinesWithoutAbort: only an
// approved signer reaches the apply handler, so a payload that can never
// decode must quarantine as consumed instead of aborting the pull batch.
func TestApplyWipEventMalformedPayloadQuarantinesWithoutAbort(t *testing.T) {
	ctx := context.Background()
	st, device := newSyncStore(t)
	signing := addRemoteDeviceForApplyTest(t, st, "device-wip", "approved")
	now := time.Now().UnixMilli()
	bad := state.Event{
		ID:          "evt_bad_wip",
		DeviceID:    "device-wip",
		Seq:         1,
		HLC:         now << hlcLogicalBits,
		Type:        EventRepoWipPushed,
		PayloadJSON: `{"path":`,
	}
	bad.ContentHash = state.ContentHash(bad.PayloadJSON)
	sig, err := devicekeys.Sign(signing.Private, "devstrap:event:v2", state.EventSignaturePayloadV2(bad))
	if err != nil {
		t.Fatal(err)
	}
	bad.DeviceSig = sig
	good := projEvent(t, device.ID, EventProjectAdded, now+1, "work/acme/after-wip", "github.com/acme/after-wip")
	good.Seq = 1
	_, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{bad, good}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Quarantined != 1 || stats.CursorHeld {
		t.Fatalf("stats=%+v, want malformed wip event quarantined as consumed", stats)
	}
	if _, err := st.ProjectByPath(ctx, "work/acme/after-wip"); err != nil {
		t.Fatalf("batch must continue past malformed wip event: %v", err)
	}
	conflicts, err := st.OpenConflictsByType(ctx, ConflictEventVerification)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range conflicts {
		var d eventVerificationConflictDetails
		if json.Unmarshal([]byte(c.DetailsJSON), &d) == nil && d.EventID == "evt_bad_wip" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want a verification-failure conflict for evt_bad_wip, got %#v", conflicts)
	}
}

// TestApplyWipEventUnsafePathQuarantinesWithoutAbort mirrors
// TestApplyGitstateEventUnsafePathQuarantinesWithoutAbort: a verified event
// whose path can never resolve (path escape) must quarantine as consumed
// like the gitstate/draft/env unsafe-path case, not abort the batch.
func TestApplyWipEventUnsafePathQuarantinesWithoutAbort(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	signing := addRemoteDeviceForApplyTest(t, st, "device-wip", "approved")
	now := time.Now().UnixMilli()
	ev := signedWipEvent(t, signing, "evt_wip_escape", "device-wip", 1, now, WipPayload{
		Path: "../escape", SHA: "aaa",
	})
	_, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{ev}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Quarantined != 1 || stats.CursorHeld {
		t.Fatalf("stats=%+v, want the unsafe-path wip event quarantined as consumed", stats)
	}
}

// TestNewWipPushedEventStampsTypeAndContentHash pins the unsigned-constructor
// contract (mirrors NewGitstateEvent/NewDraftSnapshotEvent/NewEnvProfileEvent):
// InsertLocalEvent stamps HLC/seq/device id/signature, so the constructor
// itself only sets type, payload, and content hash.
func TestNewWipPushedEventStampsTypeAndContentHash(t *testing.T) {
	raw, err := json.Marshal(WipPayload{Path: "work/acme/proj", Ref: "refs/devstrap/wip/dev_x/work/acme/proj", SHA: "aaa"})
	if err != nil {
		t.Fatal(err)
	}
	ev := NewWipPushedEvent(string(raw))
	if ev.Type != EventRepoWipPushed {
		t.Fatalf("Type = %q, want %q", ev.Type, EventRepoWipPushed)
	}
	if ev.PayloadJSON != string(raw) {
		t.Fatalf("PayloadJSON = %q, want %q", ev.PayloadJSON, string(raw))
	}
	if ev.ContentHash != state.ContentHash(string(raw)) {
		t.Fatalf("ContentHash = %q, want the payload's content hash", ev.ContentHash)
	}
}
