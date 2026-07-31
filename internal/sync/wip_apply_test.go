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

func wipDroppedEvent(t *testing.T, id, dropper string, seq, hlc int64, payload WipDroppedPayload) state.Event {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return state.Event{
		ID: id, DeviceID: dropper, Seq: seq, HLC: hlc << hlcLogicalBits,
		Type: EventRepoWipDropped, PayloadJSON: string(raw), ContentHash: state.ContentHash(string(raw)),
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

func TestWipTombstoneBlocksOlderPushedResurrection(t *testing.T) {
	base := time.Now().UnixMilli()
	path := "work/acme/proj"
	push := func(t *testing.T, id string, seq, offset int64, sha string) state.Event {
		return wipEvent(t, id, "dev_owner", seq, base+offset, WipPayload{Path: path, SHA: sha})
	}
	drop := func(t *testing.T, id string, seq, offset int64, sha string) state.Event {
		return wipDroppedEvent(t, id, "dev_dropper", seq, base+offset, WipDroppedPayload{Path: path, DeviceID: "dev_owner", SHA: sha})
	}
	tests := []struct {
		name string
		evs  []state.Event
		live bool
		sha  string
	}{
		{"pushed positive control", []state.Event{push(t, "p1", 1, 5, "abc")}, true, "abc"},
		{"drop then older same push", []state.Event{drop(t, "d2", 1, 9, "abc"), push(t, "p2", 1, 5, "abc")}, false, ""},
		{"newer same push revives", []state.Event{drop(t, "d3", 1, 9, "abc"), push(t, "p3a", 1, 5, "abc"), push(t, "p3b", 2, 12, "abc")}, true, "abc"},
		{"concurrent different sha", []state.Event{drop(t, "d4", 1, 9, "abc"), push(t, "p4", 1, 7, "def")}, true, "def"},
		{"equal hlc different sha", []state.Event{drop(t, "d5", 1, 9, "abc"), push(t, "p5", 1, 9, "def")}, true, "def"},
		// The repeated semantic delivery has an older cross-stream HLC. MAX
		// must retain 9, so an equal-HLC same-SHA push stays dead.
		{"drop delivered twice", []state.Event{drop(t, "d6a", 1, 9, "abc"), drop(t, "d6b", 2, 5, "abc"), push(t, "p6", 1, 9, "abc")}, false, ""},
		// A drop with NO prior push inserts a tombstone-only row
		// (observed_at_hlc = 0, empty sha). Routine, not exotic: the drop
		// simply arrives first, or a snapshot-bootstrapped device never sees a
		// pushed event that was already compacted away. That empty sha differs
		// from dropped_sha, so an ungated sha escape renders the tombstone as a
		// phantom pending WIP carrying no ref — surfaced by `wip status`,
		// `status --all-devices`, and `doctor`, and unusable by
		// `wip show`/`apply`. An unobserved push cannot be the racing push the
		// escape exists for.
		{"drop with no prior push", []state.Event{drop(t, "d7", 1, 9, "abc")}, false, ""},
		// ...and the tombstone must still bite once that push arrives late.
		{"drop with no prior push then older push", []state.Event{drop(t, "d8", 1, 9, "abc"), push(t, "p8", 1, 5, "abc")}, false, ""},
		// TWO drops of DIFFERENT shas, newest-HLC first. Reachable with only
		// honest CLI events: A pushes abc, B drops it, A re-pushes def, C drops
		// that — then a fourth device pulls the two drops out of order. If
		// dropped_at_hlc took MAX while dropped_sha was last-write-wins, the row
		// would converge on the newer HLC beside the OLDER drop's sha, a pair no
		// event ever carried, and the push of the sha that WAS deleted would
		// read live forever. The pair must move together.
		{"two drops different shas newest first", []state.Event{
			drop(t, "d9a", 1, 12, "def"), drop(t, "d9b", 2, 9, "abc"), push(t, "p9", 1, 10, "def"),
		}, false, ""},
		// The same, delivered oldest-drop-first, must converge identically —
		// otherwise devices disagree on liveness depending on pull order.
		{"two drops different shas oldest first", []state.Event{
			drop(t, "d10a", 1, 9, "abc"), drop(t, "d10b", 2, 12, "def"), push(t, "p10", 1, 10, "def"),
		}, false, ""},
		// An UNLEASED drop (the already-gone path publishes SHA "") proved the
		// ref absent but not which sha was there, so it must be sha-agnostic: a
		// prior observation of any sha at or below its HLC is dead. Treating ""
		// as just another sha would make every real sha differ from it and
		// resurrect the row on every device.
		{"unleased drop outranks a different sha", []state.Event{
			drop(t, "d11", 1, 9, ""), push(t, "p11", 1, 5, "abc"),
		}, false, ""},
		// ...but an unleased drop still must not bury a genuinely NEWER push.
		{"unleased drop does not bury a newer push", []state.Event{
			drop(t, "d12", 1, 9, ""), push(t, "p12", 1, 12, "abc"),
		}, true, "abc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			st, _ := newSyncStore(t)
			// Apply one delivery window at a time. ApplyEvents sorts within a
			// window, while this regression must force cross-stream arrival
			// orders that only separate sync pulls can produce.
			for _, ev := range tt.evs {
				if _, err := ApplyEvents(ctx, st, []state.Event{ev}); err != nil {
					t.Fatal(err)
				}
			}
			rows, err := st.DeviceWipForProject(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			if tt.live {
				if len(rows) != 1 || rows[0].SHA != tt.sha {
					t.Fatalf("rows=%#v, want live sha %q", rows, tt.sha)
				}
			} else if len(rows) != 0 {
				t.Fatalf("rows=%#v, want tombstoned row absent", rows)
			}
		})
	}
}

func TestWipDroppedAppliesForPeerOwner(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	base := time.Now().UnixMilli()
	path := "work/acme/proj"
	events := []state.Event{
		wipEvent(t, "owner-push", "dev_owner", 1, base+5, WipPayload{Path: path, SHA: "abc"}),
		wipEvent(t, "dropper-push", "dev_dropper", 1, base+5, WipPayload{Path: path, SHA: "xyz"}),
		wipDroppedEvent(t, "peer-drop", "dev_dropper", 2, base+9, WipDroppedPayload{Path: path, DeviceID: "dev_owner", SHA: "abc"}),
	}
	if _, err := ApplyEvents(ctx, st, events); err != nil {
		t.Fatal(err)
	}
	rows, err := st.DeviceWipForProject(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].DeviceID != "dev_dropper" || rows[0].SHA != "xyz" {
		t.Fatalf("rows=%#v, want only dropper's own live row", rows)
	}
}

// TestApplyWipEventRejectsEmptySHA pins P9-WIP-01. `wip drop` and the automatic
// `wip gc` lease their delete to the mirror row's sha
// (--force-with-lease=<ref>:<sha>), and git.Runner.DeleteRef OMITS the lease
// entirely when that value is empty — silently turning a compare-and-delete into
// an unconditional delete of whatever recovery snapshot the remote holds. That
// is exactly the loss the lease exists to prevent, and `wip gc` runs unattended
// on every convergence cycle.
//
// The apply path already validates the payload's path; the sha is the other half
// of the mirror's safety contract, so an empty one must never reach the mirror.
func TestApplyWipEventRejectsEmptySHA(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	signing := addRemoteDeviceForApplyTest(t, st, "device-wip", "approved")
	now := time.Now().UnixMilli()
	ev := signedWipEvent(t, signing, "evt_wip_nosha", "device-wip", 1, now, WipPayload{
		Path: "work/acme/proj", Ref: "refs/devstrap/wip/device-wip/work/acme/proj",
		SHA: "", BaseSHA: "def456", CapturedAt: "2026-07-31T00:00:00Z",
	})
	_, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{ev}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Quarantined != 1 {
		t.Fatalf("stats=%+v, want the sha-less wip event quarantined rather than mirrored", stats)
	}
	rows, err := st.DeviceWipForProject(ctx, "work/acme/proj")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows=%#v, want NO mirrored row: a row with an empty sha makes the next drop/gc an unleased, unconditional delete", rows)
	}
}
