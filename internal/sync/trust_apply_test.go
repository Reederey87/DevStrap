package sync

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/state"
)

func signedDeviceTrustEvent(t *testing.T, signing devicekeys.SigningIdentity, id, signer string, seq, hlc int64, typ, target string) state.Event {
	t.Helper()
	raw, err := json.Marshal(DeviceTrustPayload{DeviceID: target})
	if err != nil {
		t.Fatal(err)
	}
	ev := state.Event{
		ID:          id,
		DeviceID:    signer,
		Seq:         seq,
		HLC:         hlc << hlcLogicalBits,
		Type:        typ,
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

func deviceTrustState(t *testing.T, st *state.Store, id string) string {
	t.Helper()
	devices, err := st.ListDevices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range devices {
		if d.ID == id {
			return d.TrustState
		}
	}
	return ""
}

// TestApplyDeviceRevokedFlipsTrustAndFlagsRotation: a revoke signed by an
// approved device flips the target and flags encrypted bindings for rotation.
func TestApplyDeviceRevokedFlipsTrustAndFlagsRotation(t *testing.T) {
	ctx := context.Background()
	st, device := newSyncStore(t)
	signingA := addRemoteDeviceForApplyTest(t, st, "device-a", "approved")
	addRemoteDeviceForApplyTest(t, st, "device-b", "approved")
	now := time.Now().UnixMilli()

	// Seed a project + captured profile so there is a binding to flag.
	add := projEvent(t, device.ID, EventProjectAdded, now, "work/acme/api", "github.com/acme/api")
	env := signedEnvProfileEvent(t, signingA, "evt_env_seed", "device-a", 1, now+1, EnvProfilePayload{
		Path: "work/acme/api", Profile: "default", Provider: "devstrap_encrypted",
		Mode: "hydrate_or_runtime", BlobRef: "age_blob:seed", VarNames: []string{"API_TOKEN"},
	})
	if _, err := ApplyEvents(ctx, st, []state.Event{add, env}); err != nil {
		t.Fatal(err)
	}

	revoke := signedDeviceTrustEvent(t, signingA, "evt_revoke_b", "device-a", 2, now+2, EventDeviceRevoked, "device-b")
	_, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{revoke}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Quarantined != 0 {
		t.Fatalf("stats=%+v, want the revoke applied", stats)
	}
	if got := deviceTrustState(t, st, "device-b"); got != "revoked" {
		t.Fatalf("device-b trust=%q, want revoked", got)
	}
	n, err := st.CountSecretBindingsNeedingRotation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("needs_rotation count=%d, want 1", n)
	}
}

// TestApplyDeviceTrustReplayDoesNotReflagRotation: after the operator rotates
// and clears the flags, a second trust event for the SAME already-revoked
// target (changed=false) must not re-flag.
func TestApplyDeviceTrustReplayDoesNotReflagRotation(t *testing.T) {
	ctx := context.Background()
	st, device := newSyncStore(t)
	signingA := addRemoteDeviceForApplyTest(t, st, "device-a", "approved")
	addRemoteDeviceForApplyTest(t, st, "device-b", "approved")
	now := time.Now().UnixMilli()

	add := projEvent(t, device.ID, EventProjectAdded, now, "work/acme/api", "github.com/acme/api")
	env := signedEnvProfileEvent(t, signingA, "evt_env_seed", "device-a", 1, now+1, EnvProfilePayload{
		Path: "work/acme/api", Profile: "default", Provider: "devstrap_encrypted",
		Mode: "hydrate_or_runtime", BlobRef: "age_blob:seed", VarNames: []string{"API_TOKEN"},
	})
	revoke := signedDeviceTrustEvent(t, signingA, "evt_revoke_b", "device-a", 2, now+2, EventDeviceRevoked, "device-b")
	if _, err := ApplyEvents(ctx, st, []state.Event{add, env, revoke}); err != nil {
		t.Fatal(err)
	}
	project, err := st.ProjectByPath(ctx, "work/acme/api")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClearRotationForProject(ctx, project.ID); err != nil {
		t.Fatal(err)
	}

	// A later `lost` for the already-revoked target is a sticky no-op.
	lost := signedDeviceTrustEvent(t, signingA, "evt_lost_b", "device-a", 3, now+3, EventDeviceLost, "device-b")
	if _, err := ApplyEvents(ctx, st, []state.Event{lost}); err != nil {
		t.Fatal(err)
	}
	if got := deviceTrustState(t, st, "device-b"); got != "revoked" {
		t.Fatalf("device-b trust=%q, want revoked (sticky, lost no-op)", got)
	}
	n, err := st.CountSecretBindingsNeedingRotation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("needs_rotation count=%d, want 0 (no re-flag on changed=false)", n)
	}
}

// TestApplyDeviceRevokedUnknownTargetCreatesRevokedPlaceholder: the target may
// be a device this store has never seen (device records do not sync); the
// apply creates the placeholder and revokes it, so its future events
// quarantine.
func TestApplyDeviceRevokedUnknownTargetCreatesRevokedPlaceholder(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	signingA := addRemoteDeviceForApplyTest(t, st, "device-a", "approved")
	now := time.Now().UnixMilli()
	revoke := signedDeviceTrustEvent(t, signingA, "evt_revoke_c", "device-a", 1, now, EventDeviceRevoked, "device-never-seen")
	if _, err := ApplyEvents(ctx, st, []state.Event{revoke}); err != nil {
		t.Fatal(err)
	}
	if got := deviceTrustState(t, st, "device-never-seen"); got != "revoked" {
		t.Fatalf("unknown target trust=%q, want revoked placeholder", got)
	}
}

// TestApplyDeviceRevokedFromUntrustedSignerQuarantines: device.revoked is
// mustVerify — a pending (unpinned) signer's revoke quarantines instead of
// flipping trust.
func TestApplyDeviceRevokedFromUntrustedSignerQuarantines(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	signingP := addRemoteDeviceForApplyTest(t, st, "device-pending", "pending")
	addRemoteDeviceForApplyTest(t, st, "device-b", "approved")
	now := time.Now().UnixMilli()
	revoke := signedDeviceTrustEvent(t, signingP, "evt_revoke_bad", "device-pending", 1, now, EventDeviceRevoked, "device-b")
	_, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{revoke}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Quarantined != 1 {
		t.Fatalf("stats=%+v, want the untrusted revoke quarantined", stats)
	}
	if got := deviceTrustState(t, st, "device-b"); got != "approved" {
		t.Fatalf("device-b trust=%q, want approved (untrusted revoke rejected)", got)
	}
}

// TestApplyDeviceTrustLocalTargetIsNoOp: a remote event naming THIS device
// never flips local trust — the hub cannot talk a device into distrusting
// itself.
func TestApplyDeviceTrustLocalTargetIsNoOp(t *testing.T) {
	ctx := context.Background()
	st, device := newSyncStore(t)
	signingA := addRemoteDeviceForApplyTest(t, st, "device-a", "approved")
	now := time.Now().UnixMilli()
	revoke := signedDeviceTrustEvent(t, signingA, "evt_revoke_local", "device-a", 1, now, EventDeviceRevoked, device.ID)
	_, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{revoke}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Quarantined != 0 {
		t.Fatalf("stats=%+v, want event consumed as a no-op", stats)
	}
	if got := deviceTrustState(t, st, device.ID); got == "revoked" || got == "lost" {
		t.Fatalf("local device trust=%q, must never flip from a remote event", got)
	}
}

// TestApplyMutualRevocationSingleBatchDeterministic: [A revokes B, B revokes A]
// in one batch converges to the HLC-earlier revoke regardless of input order —
// the winner flips its target first, so the loser's event then fails
// verification (its signer is revoked) and quarantines.
func TestApplyMutualRevocationSingleBatchDeterministic(t *testing.T) {
	now := time.Now().UnixMilli()
	for _, order := range []string{"a-first", "b-first"} {
		t.Run(order, func(t *testing.T) {
			ctx := context.Background()
			st, _ := newSyncStore(t)
			signingA := addRemoteDeviceForApplyTest(t, st, "device-a", "approved")
			signingB := addRemoteDeviceForApplyTest(t, st, "device-b", "approved")
			revokeB := signedDeviceTrustEvent(t, signingA, "evt_a_revokes_b", "device-a", 1, now, EventDeviceRevoked, "device-b")
			revokeA := signedDeviceTrustEvent(t, signingB, "evt_b_revokes_a", "device-b", 1, now+1, EventDeviceRevoked, "device-a")
			batch := []state.Event{revokeB, revokeA}
			if order == "b-first" {
				batch = []state.Event{revokeA, revokeB}
			}
			_, stats, err := ApplyEventsWithStats(ctx, st, batch, nil)
			if err != nil {
				t.Fatal(err)
			}
			// A's revoke has the earlier HLC: it applies, B flips to revoked,
			// then B's counter-revoke fails verification and quarantines.
			if got := deviceTrustState(t, st, "device-b"); got != "revoked" {
				t.Fatalf("device-b trust=%q, want revoked (HLC-earlier revoke wins)", got)
			}
			if got := deviceTrustState(t, st, "device-a"); got != "approved" {
				t.Fatalf("device-a trust=%q, want approved (counter-revoke quarantined)", got)
			}
			if stats.Quarantined != 1 {
				t.Fatalf("stats=%+v, want exactly the counter-revoke quarantined", stats)
			}
		})
	}
}

// TestApplyDeviceRevokedThenTargetEventQuarantines: once the revoke applies,
// the revoked device's later events in the SAME batch quarantine while other
// devices' events keep applying.
func TestApplyDeviceRevokedThenTargetEventQuarantines(t *testing.T) {
	ctx := context.Background()
	st, device := newSyncStore(t)
	signingA := addRemoteDeviceForApplyTest(t, st, "device-a", "approved")
	signingB := addRemoteDeviceForApplyTest(t, st, "device-b", "approved")
	now := time.Now().UnixMilli()
	revoke := signedDeviceTrustEvent(t, signingA, "evt_revoke_b", "device-a", 1, now, EventDeviceRevoked, "device-b")
	fromB := signedProjEvent(t, signingB, "evt_b_after", "device-b", 1, now+1, EventProjectAdded, "work/acme/late", "github.com/acme/late")
	fromLocal := projEvent(t, device.ID, EventProjectAdded, now+2, "work/acme/fine", "github.com/acme/fine")
	fromLocal.Seq = 1
	_, stats, err := ApplyEventsWithStats(ctx, st, []state.Event{revoke, fromB, fromLocal}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Quarantined != 1 {
		t.Fatalf("stats=%+v, want exactly B's post-revoke event quarantined", stats)
	}
	if _, err := st.ProjectByPath(ctx, "work/acme/late"); err == nil {
		t.Fatal("revoked device's project applied, want quarantined")
	}
	if _, err := st.ProjectByPath(ctx, "work/acme/fine"); err != nil {
		t.Fatalf("unrelated device's event must still apply: %v", err)
	}
}

// TestApplyMutualRevocationCrossWindowDivergesLoudly pins the ACCEPTED
// residual (adversarial review): two bystanders that pull a mutual revocation
// in OPPOSITE windows diverge (each trusts the device whose revoke it saw
// first), but the outcome is fail-closed and LOUD on both — the loser's
// counter-revoke is preserved in an open verification quarantine, never
// silently dropped. Recovery is the documented local ceremony; note that
// re-approving the loser REPLAYS its counter-revoke (flipping the other
// device), so full recovery is re-approving BOTH — two steps, no data loss.
func TestApplyMutualRevocationCrossWindowDivergesLoudly(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UnixMilli()

	newBystander := func() (*state.Store, state.Event, state.Event) {
		st, _ := newSyncStore(t)
		signingA := addRemoteDeviceForApplyTest(t, st, "device-a", "approved")
		signingB := addRemoteDeviceForApplyTest(t, st, "device-b", "approved")
		revokeB := signedDeviceTrustEvent(t, signingA, "evt_a_revokes_b", "device-a", 1, now, EventDeviceRevoked, "device-b")
		revokeA := signedDeviceTrustEvent(t, signingB, "evt_b_revokes_a", "device-b", 1, now+1, EventDeviceRevoked, "device-a")
		return st, revokeB, revokeA
	}

	// Bystander C pulls A's revoke first (window 1), then B's (window 2).
	stC, revokeB, revokeA := newBystander()
	if _, err := ApplyEvents(ctx, stC, []state.Event{revokeB}); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEvents(ctx, stC, []state.Event{revokeA}); err != nil {
		t.Fatal(err)
	}
	// Bystander D pulls in the opposite window order.
	stD, revokeB2, revokeA2 := newBystander()
	if _, err := ApplyEvents(ctx, stD, []state.Event{revokeA2}); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEvents(ctx, stD, []state.Event{revokeB2}); err != nil {
		t.Fatal(err)
	}

	// Divergence: C revoked B and kept A; D revoked A and kept B.
	if got := deviceTrustState(t, stC, "device-b"); got != "revoked" {
		t.Fatalf("C: device-b=%q, want revoked", got)
	}
	if got := deviceTrustState(t, stC, "device-a"); got != "approved" {
		t.Fatalf("C: device-a=%q, want approved", got)
	}
	if got := deviceTrustState(t, stD, "device-a"); got != "revoked" {
		t.Fatalf("D: device-a=%q, want revoked", got)
	}
	if got := deviceTrustState(t, stD, "device-b"); got != "approved" {
		t.Fatalf("D: device-b=%q, want approved", got)
	}

	// Loudness: the losing counter-revoke is an OPEN verification conflict on
	// both bystanders, preserved for operator-driven replay — never silent.
	for name, st := range map[string]*state.Store{"C": stC, "D": stD} {
		open, err := st.OpenConflictsByType(ctx, ConflictEventVerification)
		if err != nil {
			t.Fatal(err)
		}
		if len(open) != 1 {
			t.Fatalf("%s: open verification conflicts = %d, want 1 (the quarantined counter-revoke)", name, len(open))
		}
	}
}
