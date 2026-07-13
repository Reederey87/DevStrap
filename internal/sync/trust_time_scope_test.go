package sync

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/state"
)

// TestApplyPreRevocationEventAdmittedRegardlessOfDeliveryOrder is the P7-SYNC-02
// reproduction: a legitimate event emitted while a device was still approved
// must NOT be permanently rejected just because a bystander happened to apply
// that device's later `device.revoked` event FIRST (delivery reordering by the
// untrusted hub). Before the fix the apply path checked only the device's
// CURRENT trust state, so the out-of-order bystander silently diverged from the
// fleet by dropping a valid pre-revocation event forever.
func TestApplyPreRevocationEventAdmittedRegardlessOfDeliveryOrder(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	signingA := addRemoteDeviceForApplyTest(t, st, "device-a", "approved")
	signingB := addRemoteDeviceForApplyTest(t, st, "device-b", "approved")
	now := time.Now().UnixMilli()

	// device-a legitimately adds a project while still approved (HLC = now).
	pre := signedProjEvent(t, signingA, "evt_a_pre", "device-a", 1, now, EventProjectAdded, "work/acme/pre", "github.com/acme/pre")
	// device-b later revokes device-a (HLC = now+10 — strictly after `pre`).
	revoke := signedDeviceTrustEvent(t, signingB, "evt_revoke_a", "device-b", 1, now+10, EventDeviceRevoked, "device-a")

	// Out-of-order delivery: the revocation lands FIRST, then the pre-revocation
	// event trickles in on a later pull.
	if _, err := ApplyEvents(ctx, st, []state.Event{revoke}); err != nil {
		t.Fatal(err)
	}
	if got := deviceTrustState(t, st, "device-a"); got != "revoked" {
		t.Fatalf("device-a trust=%q, want revoked", got)
	}
	_, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{pre}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Quarantined != 0 {
		t.Fatalf("stats=%+v, want the pre-revocation event admitted (not quarantined)", stats)
	}
	if _, err := st.ProjectByPath(ctx, "work/acme/pre"); err != nil {
		t.Fatalf("pre-revocation event must be admitted regardless of delivery order: %v", err)
	}
}

// TestRevokedDeviceCannotBackdatePostRevocationEvent proves RELABEL-resistance,
// NOT backdate-resistance — the name is retained for history but is misleading,
// so read this comment. The time-scoped admission rests on TWO signed, immutable
// quantities: the event's own HLC (bound into its signature) and the revocation
// boundary (an approved-signed HLC the revoked device cannot raise). This test
// proves:
//
//  1. An event at or after the boundary is rejected — a revoked device cannot
//     author new authoritative changes past its revocation.
//  2. An EXISTING post-revocation event cannot be RELABELED to look
//     pre-revocation: the HLC is inside the signed payload, so mutating it below
//     the boundary invalidates the signature and the event is rejected. The
//     receiver never trusts a mutable, hub-supplied ordering hint — only the
//     signed HLC.
//
// It does NOT prove backdate-resistance in the sense of "a revoked device cannot
// mint a BRAND-NEW forgery with a self-chosen low HLC." A revoked device
// retaining its signing key CAN freshly sign a new content event (e.g.
// project.added) with any HLC below its boundary and a VALID signature — that is
// an ACCEPTED RESIDUAL (spec/15): for an existing path the legitimate device's
// real-time event wins under HLC-monotonic reconciliation, but a genuinely NEW
// path has no contest. Grant events are excluded from that residual by Finding 1
// (see TestRevokedDeviceCannotMintKeyGrantBelowBoundary).
func TestRevokedDeviceCannotBackdatePostRevocationEvent(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	signingA := addRemoteDeviceForApplyTest(t, st, "device-a", "approved")
	signingB := addRemoteDeviceForApplyTest(t, st, "device-b", "approved")
	now := time.Now().UnixMilli()

	// device-b revokes device-a with boundary HLC = now+10.
	revoke := signedDeviceTrustEvent(t, signingB, "evt_revoke_a", "device-b", 1, now+10, EventDeviceRevoked, "device-a")
	if _, err := ApplyEvents(ctx, st, []state.Event{revoke}); err != nil {
		t.Fatal(err)
	}

	// (1) A genuinely post-revocation event (HLC = now+20 >= boundary) is
	// rejected: a revoked device gains no forward authority.
	post := signedProjEvent(t, signingA, "evt_a_post", "device-a", 1, now+20, EventProjectAdded, "work/acme/post", "github.com/acme/post")
	_, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{post}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Quarantined != 1 {
		t.Fatalf("stats=%+v, want the post-revocation event quarantined", stats)
	}
	if _, err := st.ProjectByPath(ctx, "work/acme/post"); err == nil {
		t.Fatal("post-revocation event applied, want rejected")
	}

	// (2) Take that post-revocation event and BACKDATE it: relabel its HLC to
	// now+5 (below the boundary) while keeping the signature, which was computed
	// over now+20. The HLC is signature-bound, so verification fails and the
	// forgery is rejected — the attacker cannot slip a post-boundary event under
	// the boundary by mutating the ordering field.
	backdated := signedProjEvent(t, signingA, "evt_a_backdated", "device-a", 2, now+20, EventProjectAdded, "work/acme/backdated", "github.com/acme/backdated")
	backdated.HLC = (now + 5) << hlcLogicalBits // relabel below the boundary; signature stays over now+20
	_, stats, err = ApplyEventsWithStats(ctx, st, []state.Event{backdated}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Quarantined != 1 {
		t.Fatalf("stats=%+v, want the backdated (HLC-tampered) event quarantined", stats)
	}
	if _, err := st.ProjectByPath(ctx, "work/acme/backdated"); err == nil {
		t.Fatal("backdated event applied, want rejected — HLC must be signature-bound")
	}
}

// TestRecordDeviceRevocationHLCTakesMinimum pins the delivery-order-independent
// boundary: two approved devices revoke the same target with different HLCs, and
// the recorded boundary is the EARLIEST (minimum) regardless of which revoke
// applies first — so every device in the fleet admits exactly the same set of
// pre-revocation events (the most fail-closed cut wins).
func TestRecordDeviceRevocationHLCTakesMinimum(t *testing.T) {
	now := time.Now().UnixMilli()
	for _, order := range []string{"early-first", "late-first"} {
		t.Run(order, func(t *testing.T) {
			ctx := context.Background()
			st, _ := newSyncStore(t)
			signingA := addRemoteDeviceForApplyTest(t, st, "device-a", "approved")
			signingB := addRemoteDeviceForApplyTest(t, st, "device-b", "approved")
			signingC := addRemoteDeviceForApplyTest(t, st, "device-c", "approved")

			early := signedDeviceTrustEvent(t, signingA, "evt_early", "device-a", 1, now+10, EventDeviceRevoked, "device-c")
			late := signedDeviceTrustEvent(t, signingB, "evt_late", "device-b", 1, now+50, EventDeviceRevoked, "device-c")
			batch := []state.Event{early, late}
			if order == "late-first" {
				batch = []state.Event{late, early}
			}
			if _, err := ApplyEvents(ctx, st, batch); err != nil {
				t.Fatal(err)
			}

			// An event from device-c between the two boundaries (now+10 < now+30 <
			// now+50) must be REJECTED: the earliest revocation is authoritative.
			between := signedProjEvent(t, signingC, "evt_c_between", "device-c", 1, now+30, EventProjectAdded, "work/acme/between", "github.com/acme/between")
			_, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{between}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if stats.Quarantined != 1 {
				t.Fatalf("stats=%+v, want the between-boundaries event rejected (min boundary wins)", stats)
			}
			// An event strictly before the earliest boundary is still admitted.
			before := signedProjEvent(t, signingC, "evt_c_before", "device-c", 2, now+5, EventProjectAdded, "work/acme/before", "github.com/acme/before")
			if _, err := ApplyEvents(ctx, st, []state.Event{before}); err != nil {
				t.Fatal(err)
			}
			if _, err := st.ProjectByPath(ctx, "work/acme/before"); err != nil {
				t.Fatalf("event before earliest boundary must be admitted: %v", err)
			}
		})
	}
}

// signedGrantEvent builds a validly-signed device.key.granted carrier event for
// a given signing identity at an (unshifted) HLC. The payload is well-formed but
// its WCK material is irrelevant to these tests: the event is rejected at the
// verification gate (P7-SYNC-02 Finding 1) before RecordKeyGrantTx/IngestGrant
// ever runs, so no real age-wrapped key is needed.
func signedGrantEvent(t *testing.T, signing devicekeys.SigningIdentity, id, dev string, seq, hlc, epoch int64) state.Event {
	t.Helper()
	raw, err := json.Marshal(DeviceKeyGrant{Epoch: epoch, KID: "kid-forged", Recipient: "age1victim", WrappedKey: "d2Vsc2VjcmV0"})
	if err != nil {
		t.Fatal(err)
	}
	ev := state.Event{
		ID:          id,
		DeviceID:    dev,
		Seq:         seq,
		HLC:         hlc << hlcLogicalBits,
		Type:        EventDeviceKeyGranted,
		PayloadJSON: string(raw),
		ContentHash: state.ContentHash(string(raw)),
	}
	sig, err := devicekeys.Sign(signing.Private, "devstrap:event:v2", state.EventSignaturePayloadV2(ev))
	if err != nil {
		t.Fatal(err)
	}
	ev.DeviceSig = sig
	return ev
}

// TestRevokedDeviceCannotMintKeyGrantBelowBoundary is the P7-SYNC-02 Finding 1
// guard (fable-5 review). Grant events (device.key.granted) are EXCLUDED from the
// time-scoped exemption because they have forward-looking side effects: admitting
// one writes an attacker-chosen Workspace Content Key into the keyring that every
// peer then seals FUTURE events under. A revoked device retaining its signing key
// must NOT be able to mint a fresh, validly-signed grant backdated below its
// revocation boundary and have it admitted — even though the SAME-shaped attack
// against an ordinary content event IS admitted (that is the accepted residual,
// bounded by highest-wins reconciliation; a grant has no such contest).
func TestRevokedDeviceCannotMintKeyGrantBelowBoundary(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	signingA := addRemoteDeviceForApplyTest(t, st, "device-a", "approved")
	signingB := addRemoteDeviceForApplyTest(t, st, "device-b", "approved")
	now := time.Now().UnixMilli()

	// device-b revokes device-a with boundary HLC = now+10.
	revoke := signedDeviceTrustEvent(t, signingB, "evt_revoke_a", "device-b", 1, now+10, EventDeviceRevoked, "device-a")
	if _, err := ApplyEvents(ctx, st, []state.Event{revoke}); err != nil {
		t.Fatal(err)
	}
	if got := deviceTrustState(t, st, "device-a"); got != "revoked" {
		t.Fatalf("device-a trust=%q, want revoked", got)
	}

	// Control: a CONTENT event from the revoked device backdated below the
	// boundary (HLC = now+5) IS admitted via the time-scoped exemption. This
	// proves the boundary/exemption machinery is live for the same device+HLC,
	// isolating the grant exclusion as the ONLY reason the grant below is
	// rejected (not a stale boundary or a signature problem).
	content := signedProjEvent(t, signingA, "evt_a_content", "device-a", 1, now+5, EventProjectAdded, "work/acme/content", "github.com/acme/content")
	if _, err := ApplyEvents(ctx, st, []state.Event{content}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ProjectByPath(ctx, "work/acme/content"); err != nil {
		t.Fatalf("control: a backdated content event must be admitted (proves the exemption is live for this device/HLC): %v", err)
	}

	// The attack: the revoked device mints a FRESH, validly-signed grant at a
	// higher epoch, backdated to HLC = now+5 (strictly below the boundary). It
	// must be REJECTED at the verification gate — quarantined, never ingested.
	grant := signedGrantEvent(t, signingA, "evt_a_grant", "device-a", 2, now+5, 99)
	_, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{grant}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Quarantined != 1 {
		t.Fatalf("stats=%+v, want the backdated grant quarantined (grants are excluded from the time-scoped exemption)", stats)
	}
	// The grant carrier must not have been inserted into the event log
	// (verification runs before insert, so RecordKeyGrantTx never fires).
	if _, err := st.EventByID(ctx, "evt_a_grant"); err == nil {
		t.Fatal("grant carrier event was inserted; it must be rejected before insert")
	}
}

// signedConflictCreatedEvent builds a validly-signed conflict.created event at
// an (unshifted) HLC. The payload is a minimal placeholder — conflict.created
// is not one of the six allowlisted content types, so these tests expect
// rejection at the verification gate before payload semantics matter.
func signedConflictCreatedEvent(t *testing.T, signing devicekeys.SigningIdentity, id, dev string, seq, hlc int64) state.Event {
	t.Helper()
	raw := []byte(`{}`)
	ev := state.Event{
		ID:          id,
		DeviceID:    dev,
		Seq:         seq,
		HLC:         hlc << hlcLogicalBits,
		Type:        EventConflictCreated,
		PayloadJSON: string(raw),
		ContentHash: state.ContentHash(string(raw)),
	}
	sig, err := devicekeys.Sign(signing.Private, "devstrap:event:v2", state.EventSignaturePayloadV2(ev))
	if err != nil {
		t.Fatal(err)
	}
	ev.DeviceSig = sig
	return ev
}

// TestRevokedDeviceCannotBackdateConflictEventBelowBoundary is the round-2
// independent-review guard (positive-allowlist finding): a negative exclusion
// ("everything except trust events and key grants") would have silently
// admitted a backdated conflict.created from a revoked device, since conflict
// events are neither trust events nor key grants. The positive allowlist
// (isTimeScopedContentEvent) only covers the six documented project/env/draft
// content types, so conflict.created — like device.key.granted — always
// requires CURRENT approval and must be rejected here exactly like the grant
// case in TestRevokedDeviceCannotMintKeyGrantBelowBoundary.
func TestRevokedDeviceCannotBackdateConflictEventBelowBoundary(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	signingA := addRemoteDeviceForApplyTest(t, st, "device-a", "approved")
	signingB := addRemoteDeviceForApplyTest(t, st, "device-b", "approved")
	now := time.Now().UnixMilli()

	// device-b revokes device-a with boundary HLC = now+10.
	revoke := signedDeviceTrustEvent(t, signingB, "evt_revoke_a", "device-b", 1, now+10, EventDeviceRevoked, "device-a")
	if _, err := ApplyEvents(ctx, st, []state.Event{revoke}); err != nil {
		t.Fatal(err)
	}

	// Control: a CONTENT event from the revoked device backdated below the
	// boundary IS admitted (same control as the grant test).
	content := signedProjEvent(t, signingA, "evt_a_content2", "device-a", 1, now+5, EventProjectAdded, "work/acme/content2", "github.com/acme/content2")
	if _, err := ApplyEvents(ctx, st, []state.Event{content}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ProjectByPath(ctx, "work/acme/content2"); err != nil {
		t.Fatalf("control: a backdated content event must be admitted: %v", err)
	}

	// The attack: a backdated conflict.created from the revoked device must be
	// REJECTED — conflict events are not in the positive allowlist.
	conflict := signedConflictCreatedEvent(t, signingA, "evt_a_conflict", "device-a", 2, now+5)
	_, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{conflict}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Quarantined != 1 {
		t.Fatalf("stats=%+v, want the backdated conflict event quarantined (conflict events are not in the positive allowlist)", stats)
	}
	if _, err := st.EventByID(ctx, "evt_a_conflict"); err == nil {
		t.Fatal("conflict event was inserted; it must be rejected before insert")
	}
}

// TestReapprovalClearsRevocationBoundary is the P7-SYNC-02 Finding 2 guard
// (fable-5 review). The revocation boundary is the MINIMUM revocation HLC, so
// without clearing it on re-approval it would span MULTIPLE revocation
// generations. Re-approval (`devices approve`) is the prescribed
// mutual-revocation recovery path, so a revoke -> re-approve -> revoke-again
// sequence must record a FRESH boundary the second time; otherwise a legitimate
// event from the device's SECOND approved window (B1 < HLC < R2) is wrongly
// rejected on later delivery — reintroducing the exact delivery-order divergence
// this finding closes.
func TestReapprovalClearsRevocationBoundary(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	signingA := addRemoteDeviceForApplyTest(t, st, "device-a", "approved")
	signingB := addRemoteDeviceForApplyTest(t, st, "device-b", "approved")
	now := time.Now().UnixMilli()

	// First revocation: boundary B1 = now+10.
	revoke1 := signedDeviceTrustEvent(t, signingB, "evt_revoke_a1", "device-b", 1, now+10, EventDeviceRevoked, "device-a")
	if _, err := ApplyEvents(ctx, st, []state.Event{revoke1}); err != nil {
		t.Fatal(err)
	}
	if got := deviceTrustState(t, st, "device-a"); got != "revoked" {
		t.Fatalf("after first revoke device-a trust=%q, want revoked", got)
	}

	// Local re-approval (the recovery path). This must CLEAR revoked_at_hlc.
	if err := st.SetDeviceTrustState(ctx, "device-a", "approved"); err != nil {
		t.Fatal(err)
	}

	// Second revocation, strictly LATER: boundary R2 = now+50. With the boundary
	// cleared on re-approval, the recorded boundary is R2 (not min(B1, R2) = B1).
	revoke2 := signedDeviceTrustEvent(t, signingB, "evt_revoke_a2", "device-b", 2, now+50, EventDeviceRevoked, "device-a")
	if _, err := ApplyEvents(ctx, st, []state.Event{revoke2}); err != nil {
		t.Fatal(err)
	}
	if got := deviceTrustState(t, st, "device-a"); got != "revoked" {
		t.Fatalf("after second revoke device-a trust=%q, want revoked", got)
	}

	// An event device-a emitted during its SECOND approved window (B1 < now+30 <
	// R2) must be ADMITTED: it is below the fresh boundary R2. If the boundary
	// were still stuck at B1 (Finding-2 bug), now+30 >= B1 would reject it.
	second := signedProjEvent(t, signingA, "evt_a_second", "device-a", 1, now+30, EventProjectAdded, "work/acme/second", "github.com/acme/second")
	_, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Quarantined != 0 {
		t.Fatalf("stats=%+v, want the second-approved-window event admitted (boundary must be cleared on re-approval)", stats)
	}
	if _, err := st.ProjectByPath(ctx, "work/acme/second"); err != nil {
		t.Fatalf("second-approved-window event must be admitted after re-approval clears the boundary: %v", err)
	}
}
