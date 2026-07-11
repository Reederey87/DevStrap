package cli

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/viper"
)

// Issue #134: a `devices revoke` whose WCK rotation fails must (a) preflight-
// name the malformed remaining recipient, (b) persist the owed rotation, and
// (c) have the next sync cycle's rotation gate retry it — even with periodic
// rotation disabled — clearing the marker on success.

// openTestStore opens the state DB under home for assertions.
func openTestStore(t *testing.T, home string) *state.Store {
	t.Helper()
	opts := &options{v: viper.New()}
	opts.v.Set("home", home)
	st, err := state.Open(context.Background(), opts.paths().StateDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeStore(st) })
	return st
}

func TestDeviceRevokeRotationFailureMarksPendingAndSyncRetries(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	ctx := context.Background()
	// rotateTestHome: init + EnsureBootstrap(epoch 1) + one approved peer with
	// VALID keys ("dev_rotate_peer") + a captured env binding.
	home, root := rotateTestHome(t)

	st := openTestStore(t, home)
	// A second approved bystander with a MALFORMED age recipient: Rotate
	// wrap-first fails on it, which is exactly the #134 failure class.
	if err := st.UpsertDevice(ctx, state.Device{
		ID: "dev_badrec", Name: "badrec", OS: "linux", Arch: "arm64",
		PublicKey: "not-an-age-recipient", SigningPublicKey: "irrelevant", TrustState: "approved",
	}); err != nil {
		t.Fatal(err)
	}

	_, stderr, err := executeForTest("--home", home, "--root", root, "devices", "revoke", "dev_rotate_peer")
	if err != nil {
		t.Fatalf("revoke: %v (%s)", err, stderr)
	}
	// (a) The preflight names the malformed REMAINING device before the flip.
	if !strings.Contains(stderr, "remaining device dev_badrec has a malformed age recipient") {
		t.Fatalf("stderr = %q, want preflight warning naming dev_badrec", stderr)
	}
	// The rotation itself failed loudly and promised the auto-retry.
	if !strings.Contains(stderr, "workspace key rotation FAILED") {
		t.Fatalf("stderr = %q, want rotation FAILED warning", stderr)
	}
	if !strings.Contains(stderr, "retries the rotation automatically") {
		t.Fatalf("stderr = %q, want auto-retry promise", stderr)
	}
	// (b) The owed rotation is persisted with the epoch active at failure.
	raw, ok, err := st.GetLocalMeta(ctx, wckRotationPendingMetaKey)
	if err != nil || !ok {
		t.Fatalf("GetLocalMeta(%s) = %q, %v, %v; want persisted marker", wckRotationPendingMetaKey, raw, ok, err)
	}
	var rec wckRotationPendingRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Epoch != 1 || rec.Since.IsZero() {
		t.Fatalf("pending record = %+v; want epoch 1 with a timestamp", rec)
	}
	if epoch, err := st.CurrentKeyEpoch(ctx); err != nil || epoch != 1 {
		t.Fatalf("CurrentKeyEpoch = %d, %v; want 1 (rotation failed)", epoch, err)
	}

	// Fix the bystander's recipient, then run the sync rotation gate with
	// periodic rotation DISABLED: the owed retry must rotate anyway (c).
	goodDev, err := deviceByID(ctx, st, "dev_rotate_peer")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertDevice(ctx, state.Device{
		ID: "dev_badrec", Name: "badrec", OS: "linux", Arch: "arm64",
		PublicKey: goodDev.PublicKey, SigningPublicKey: goodDev.SigningPublicKey, TrustState: "approved",
	}); err != nil {
		t.Fatal(err)
	}
	opts := &options{v: viper.New()}
	opts.v.Set("home", home)
	opts.v.Set("root", root)
	opts.v.Set("keys.rotate_max_age", "0")
	var out strings.Builder
	rotated, err := maybeRotateWorkspaceKey(ctx, &out, opts, st)
	if err != nil {
		t.Fatalf("maybeRotateWorkspaceKey: %v", err)
	}
	if !rotated {
		t.Fatal("maybeRotateWorkspaceKey did not rotate; owed retry must run even with keys.rotate_max_age=0")
	}
	if !strings.Contains(out.String(), "rotation owed since") {
		t.Fatalf("progress = %q, want owed-rotation wording", out.String())
	}
	if epoch, err := st.CurrentKeyEpoch(ctx); err != nil || epoch != 2 {
		t.Fatalf("CurrentKeyEpoch = %d, %v; want 2 after retry", epoch, err)
	}
	if _, ok, err := st.GetLocalMeta(ctx, wckRotationPendingMetaKey); err != nil || ok {
		t.Fatalf("pending marker still present (%v, %v); want cleared on success", ok, err)
	}
}

func TestRevokeContainmentMarkerCommitsWithTrustFlip(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	ctx := context.Background()
	home, _ := rotateTestHome(t)
	st := openTestStore(t, home)
	if err := st.WithTx(ctx, func(tx *state.Tx) error {
		if err := tx.SetDeviceTrustStateTx(ctx, "dev_rotate_peer", "revoked"); err != nil {
			return err
		}
		return markRevokeContainmentPendingTx(ctx, tx, "dev_rotate_peer")
	}); err != nil {
		t.Fatal(err)
	}
	if got := trustStateOf(t, home, "dev_rotate_peer"); got != "revoked" {
		t.Fatalf("trust state = %q, want revoked", got)
	}
	devices, pending, malformed, err := revokeContainmentPending(ctx, st)
	if err != nil || !pending || malformed || devices["dev_rotate_peer"].IsZero() {
		t.Fatalf("revoke containment = %v, %v, %v, %v; want target committed with trust flip", devices, pending, malformed, err)
	}
}

// TestRevokeContainmentCorruptMarkerNeverBlocksRevoke pins the post-review
// fail-direction fix: a corrupt pending record must not abort the trust-flip
// transaction (refusing a revoke over retry bookkeeping would keep a
// compromised device approved). The mark path overwrites with a fresh record
// carrying the new target.
func TestRevokeContainmentCorruptMarkerNeverBlocksRevoke(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	ctx := context.Background()
	home, _ := rotateTestHome(t)
	st := openTestStore(t, home)
	if err := st.SetLocalMeta(ctx, revokeContainmentPendingMetaKey, "{corrupt"); err != nil {
		t.Fatal(err)
	}
	if err := st.WithTx(ctx, func(tx *state.Tx) error {
		if err := tx.SetDeviceTrustStateTx(ctx, "dev_rotate_peer", "revoked"); err != nil {
			return err
		}
		return markRevokeContainmentPendingTx(ctx, tx, "dev_rotate_peer")
	}); err != nil {
		t.Fatalf("revoke tx with corrupt marker: %v (must never block the trust flip)", err)
	}
	devices, pending, malformed, err := revokeContainmentPending(ctx, st)
	if err != nil || !pending || malformed {
		t.Fatalf("containment after corrupt-marker revoke = %v/%v/%v/%v; want fresh valid record", devices, pending, malformed, err)
	}
	if devices["dev_rotate_peer"].IsZero() {
		t.Fatalf("fresh record misses the new target: %v", devices)
	}
}

func TestDeviceRevokeCurrentEpochFailureLeavesContainmentPending(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	ctx := context.Background()
	home, root := rotateTestHome(t)
	previous := currentWorkspaceKeyEpochOnRevoke
	currentWorkspaceKeyEpochOnRevoke = func(context.Context, *options, *state.Store) (int64, error) {
		return 0, errors.New("injected current epoch failure")
	}
	t.Cleanup(func() { currentWorkspaceKeyEpochOnRevoke = previous })

	_, stderr, err := executeForTest("--home", home, "--root", root, "devices", "revoke", "dev_rotate_peer")
	if err != nil {
		t.Fatalf("revoke: %v (%s)", err, stderr)
	}
	st := openTestStore(t, home)
	if got := trustStateOf(t, home, "dev_rotate_peer"); got != "revoked" {
		t.Fatalf("trust state = %q, want revoked", got)
	}
	devices, pending, malformed, err := revokeContainmentPending(ctx, st)
	if err != nil || !pending || malformed || devices["dev_rotate_peer"].IsZero() {
		t.Fatalf("revoke containment = %v, %v, %v, %v; want target pending", devices, pending, malformed, err)
	}
	if _, owed, err := wckRotationPendingSince(ctx, st); err != nil || owed {
		t.Fatalf("wck rotation marker = %v, %v; CurrentEpoch failure must rely on containment marker", owed, err)
	}
}

func TestDeviceRevokeHappyPathClearsContainment(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home, root := rotateTestHome(t)
	if _, stderr, err := executeForTest("--home", home, "--root", root, "devices", "revoke", "dev_rotate_peer"); err != nil {
		t.Fatalf("revoke: %v (%s)", err, stderr)
	}
	st := openTestStore(t, home)
	if _, pending, _, err := revokeContainmentPending(context.Background(), st); err != nil || pending {
		t.Fatalf("containment pending = %v, %v; want cleared after happy path", pending, err)
	}
}

func TestSyncResumesPendingRevokeContainment(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	ctx := context.Background()
	home, root := rotateTestHome(t)
	previous := currentWorkspaceKeyEpochOnRevoke
	currentWorkspaceKeyEpochOnRevoke = func(context.Context, *options, *state.Store) (int64, error) {
		return 0, errors.New("injected current epoch failure")
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "devices", "revoke", "dev_rotate_peer"); err != nil {
		currentWorkspaceKeyEpochOnRevoke = previous
		t.Fatalf("revoke: %v (%s)", err, stderr)
	}
	currentWorkspaceKeyEpochOnRevoke = previous

	hubFile := filepath.Join(t.TempDir(), "hub.json")
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "sync", "--hub-file", hubFile, "--namespace-only")
	if err != nil {
		t.Fatalf("sync: %v (stdout=%s stderr=%s)", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "Resumed revoke containment for 1 device(s)") {
		t.Fatalf("sync output = %q, want containment resume", stdout)
	}
	if !strings.Contains(stdout, "rotation owed since") || strings.Contains(stdout, "0001-01-01") {
		t.Fatalf("sync output = %q, want the transactional containment timestamp", stdout)
	}
	st := openTestStore(t, home)
	if _, pending, _, err := revokeContainmentPending(ctx, st); err != nil || pending {
		t.Fatalf("containment pending = %v, %v; want cleared by sync", pending, err)
	}
}

func TestTwoRevokesMergeContainmentPending(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	ctx := context.Background()
	home, root := rotateTestHome(t)
	st := openTestStore(t, home)
	peer, err := deviceByID(ctx, st, "dev_rotate_peer")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertDevice(ctx, state.Device{ID: "dev_second_peer", Name: "second", OS: "linux", Arch: "arm64", PublicKey: peer.PublicKey, SigningPublicKey: peer.SigningPublicKey, TrustState: "approved"}); err != nil {
		t.Fatal(err)
	}
	previous := currentWorkspaceKeyEpochOnRevoke
	currentWorkspaceKeyEpochOnRevoke = func(context.Context, *options, *state.Store) (int64, error) {
		return 0, errors.New("injected current epoch failure")
	}
	t.Cleanup(func() { currentWorkspaceKeyEpochOnRevoke = previous })
	for _, id := range []string{"dev_rotate_peer", "dev_second_peer"} {
		if _, stderr, err := executeForTest("--home", home, "--root", root, "devices", "revoke", id); err != nil {
			t.Fatalf("revoke %s: %v (%s)", id, err, stderr)
		}
	}
	devices, pending, malformed, err := revokeContainmentPending(ctx, st)
	if err != nil || !pending || malformed || len(devices) != 2 || devices["dev_rotate_peer"].IsZero() || devices["dev_second_peer"].IsZero() {
		t.Fatalf("merged containment = %v, %v, %v, %v; want both devices", devices, pending, malformed, err)
	}
}

func TestRevokeContainmentMalformedRecordStaysPending(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	ctx := context.Background()
	home, _ := rotateTestHome(t)
	st := openTestStore(t, home)
	if err := st.SetLocalMeta(ctx, revokeContainmentPendingMetaKey, "{garbage"); err != nil {
		t.Fatal(err)
	}
	devices, pending, malformed, err := revokeContainmentPending(ctx, st)
	if err != nil || !pending || !malformed || devices != nil {
		t.Fatalf("malformed containment = %v, %v, %v, %v; want fail-closed pending", devices, pending, malformed, err)
	}
	if err := clearRevokeContainmentPending(ctx, st, "dev_any"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.GetLocalMeta(ctx, revokeContainmentPendingMetaKey); err != nil || !ok {
		t.Fatalf("malformed marker was cleared by a by-device clear: ok=%v err=%v", ok, err)
	}
}

// TestSyncClearsMalformedContainmentMarker pins the CodeRabbit fix: a malformed
// marker previously opened the rotation gate on every sync (a Rotate storm)
// because resume only warned and never cleared it. Now a sync runs the
// device-independent containment (rotation + bindings flag + rewrap) and
// DELETES the whole malformed row, so the gate stops firing; per-device ack
// cleanup defers to hub compact.
func TestSyncClearsMalformedContainmentMarker(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	ctx := context.Background()
	home, root := rotateTestHome(t)
	// Establish a real revoke (full home + a valid, then corrupted, marker).
	previous := currentWorkspaceKeyEpochOnRevoke
	currentWorkspaceKeyEpochOnRevoke = func(context.Context, *options, *state.Store) (int64, error) {
		return 0, errors.New("injected current epoch failure")
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "devices", "revoke", "dev_rotate_peer"); err != nil {
		currentWorkspaceKeyEpochOnRevoke = previous
		t.Fatalf("revoke: %v (%s)", err, stderr)
	}
	currentWorkspaceKeyEpochOnRevoke = previous

	st := openTestStore(t, home)
	if err := st.SetLocalMeta(ctx, revokeContainmentPendingMetaKey, "{garbage"); err != nil {
		t.Fatal(err)
	}

	hubFile := filepath.Join(t.TempDir(), "hub.json")
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "sync", "--hub-file", hubFile, "--namespace-only")
	if err != nil {
		t.Fatalf("sync: %v (stdout=%s stderr=%s)", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "Recovered a malformed revoke-containment marker") {
		t.Fatalf("sync output = %q, want malformed-marker recovery", stdout)
	}
	st2 := openTestStore(t, home)
	if _, pending, _, err := revokeContainmentPending(ctx, st2); err != nil || pending {
		t.Fatalf("malformed containment still pending after sync = %v, %v; want cleared", pending, err)
	}
}

// TestMaybeRotateWarnsAndContinuesCycleOnEarlyOwedFailure (adversarial-review
// fix): an owed retry that fails EARLY (nothing recorded — the malformed-
// recipient class) must NOT fail the sync cycle, or the device.revoked event
// itself never pushes and the fleet never learns about the revoke. It warns,
// keeps the marker, and lets the cycle proceed.
func TestMaybeRotateWarnsAndContinuesCycleOnEarlyOwedFailure(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	ctx := context.Background()
	home, root := rotateTestHome(t)
	st := openTestStore(t, home)
	if err := st.UpsertDevice(ctx, state.Device{
		ID: "dev_badrec", Name: "badrec", OS: "linux", Arch: "arm64",
		PublicKey: "not-an-age-recipient", SigningPublicKey: "irrelevant", TrustState: "approved",
	}); err != nil {
		t.Fatal(err)
	}
	if err := markWCKRotationPending(ctx, st, 1); err != nil {
		t.Fatal(err)
	}
	opts := &options{v: viper.New()}
	opts.v.Set("home", home)
	opts.v.Set("root", root)
	var out strings.Builder
	rotated, err := maybeRotateWorkspaceKey(ctx, &out, opts, st)
	if rotated || err != nil {
		t.Fatalf("maybeRotateWorkspaceKey = %v, %v; want warn-and-continue on an early owed failure", rotated, err)
	}
	if !strings.Contains(out.String(), "rotation owed since") || !strings.Contains(out.String(), "still failing") {
		t.Fatalf("output = %q; want loud owed-rotation warning", out.String())
	}
	if epoch, err := st.CurrentKeyEpoch(ctx); err != nil || epoch != 1 {
		t.Fatalf("CurrentKeyEpoch = %d, %v; want unchanged 1", epoch, err)
	}
	// Marker survives for the next cycle.
	if _, ok, gerr := st.GetLocalMeta(ctx, wckRotationPendingMetaKey); gerr != nil || !ok {
		t.Fatalf("pending marker gone (%v, %v); must survive a failed retry", ok, gerr)
	}
}

// TestWCKRotationPendingSurvivesNewerEpoch (adversarial-review HIGH): a newer
// active epoch is NOT proof the revoked device was excluded — a peer that has
// not pulled the revoke yet can rotate and grant the revoked device the new
// epoch. The marker must survive until THIS device's own Rotate succeeds.
func TestWCKRotationPendingSurvivesNewerEpoch(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	ctx := context.Background()
	home, _ := rotateTestHome(t) // epoch 1 active
	st := openTestStore(t, home)
	if err := markWCKRotationPending(ctx, st, 0); err != nil { // recorded epoch 0 < active 1
		t.Fatal(err)
	}
	since, pending, err := wckRotationPendingSince(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if !pending || since.IsZero() {
		t.Fatalf("pending = %v since %v; a newer epoch must NOT resolve the owed rotation", pending, since)
	}
	if _, ok, err := st.GetLocalMeta(ctx, wckRotationPendingMetaKey); err != nil || !ok {
		t.Fatalf("marker deleted by a read (%v, %v); resolution is Rotate-only", ok, err)
	}
}

// TestKeysRotateClearsOwedRotation: the manual remedy also resolves the owed
// marker (a local Rotate always excludes locally-revoked devices).
func TestKeysRotateClearsOwedRotation(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	ctx := context.Background()
	home, root := rotateTestHome(t)
	st := openTestStore(t, home)
	if err := markWCKRotationPending(ctx, st, 1); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "keys", "rotate"); err != nil {
		t.Fatalf("keys rotate: %v (%s)", err, stderr)
	}
	if _, ok, err := st.GetLocalMeta(ctx, wckRotationPendingMetaKey); err != nil || ok {
		t.Fatalf("owed marker survives a successful keys rotate (%v, %v)", ok, err)
	}
}

// TestWCKRotationPendingMalformedRecordStaysPending: garbage in the marker
// must fail closed (still pending) — the row only exists because a rotation
// failed, and a successful retry clears it explicitly.
func TestWCKRotationPendingMalformedRecordStaysPending(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	ctx := context.Background()
	home, _ := rotateTestHome(t)
	st := openTestStore(t, home)
	if err := st.SetLocalMeta(ctx, wckRotationPendingMetaKey, "{garbage"); err != nil {
		t.Fatal(err)
	}
	_, pending, err := wckRotationPendingSince(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("malformed marker treated as not pending; must fail closed")
	}
}

func TestDoctorWarnsWCKRotationPending(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	ctx := context.Background()
	home, root := rotateTestHome(t)
	st := openTestStore(t, home)
	if err := markWCKRotationPending(ctx, st, 1); err != nil { // epoch 1 is active → still owed
		t.Fatal(err)
	}
	stdout, _, _ := executeForTest("--home", home, "--root", root, "doctor")
	if !strings.Contains(stdout, "workspace key rotation") || !strings.Contains(stdout, "owed since") {
		t.Fatalf("doctor output missing owed-rotation warning:\n%s", stdout)
	}
	if !strings.Contains(stdout, "retries the rotation automatically") {
		t.Fatalf("doctor output missing auto-retry remedy:\n%s", stdout)
	}
}

func TestDoctorWarnsRevokeContainmentPending(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	ctx := context.Background()
	home, root := rotateTestHome(t)
	st := openTestStore(t, home)
	if err := st.WithTx(ctx, func(tx *state.Tx) error {
		return markRevokeContainmentPendingTx(ctx, tx, "dev_doctor_target")
	}); err != nil {
		t.Fatal(err)
	}
	stdout, _, _ := executeForTest("--home", home, "--root", root, "doctor")
	if !strings.Contains(stdout, "revoke containment") || !strings.Contains(stdout, "dev_doctor_target since") {
		t.Fatalf("doctor output missing pending device and since-time:\n%s", stdout)
	}
	if !strings.Contains(stdout, "run devstrap sync to resume containment") {
		t.Fatalf("doctor output missing containment remedy:\n%s", stdout)
	}
}

func TestDeleteLocalMetaIdempotent(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	ctx := context.Background()
	home, _ := rotateTestHome(t)
	st := openTestStore(t, home)
	if err := st.SetLocalMeta(ctx, "test_key", "v"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteLocalMeta(ctx, "test_key"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.GetLocalMeta(ctx, "test_key"); err != nil || ok {
		t.Fatalf("row survives delete (%v, %v)", ok, err)
	}
	if err := st.DeleteLocalMeta(ctx, "test_key"); err != nil {
		t.Fatalf("deleting an absent key must be a no-op, got %v", err)
	}
}
