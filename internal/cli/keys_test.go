package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
	"github.com/Reederey87/DevStrap/internal/workspacekeys"
	"github.com/spf13/viper"
)

// P4-SEC-07 periodic rotation: `keys rotate` is a PURE Rotate — it mints
// epoch+1 and queues grants for approved devices, and must NOT carry any
// revoke side effects (no secret-rotation flags, no blob rewrap, no queued hub
// deletes). It refuses at epoch 0 (nothing to rotate).

// rotateTestHome inits a workspace, bootstraps epoch 1, enrolls one approved
// remote device, and plants a captured env binding so the no-revoke-side-
// effects contract check is meaningful (a binding exists that revoke WOULD
// flag).
func rotateTestHome(t *testing.T) (home, root string) {
	t.Helper()
	home = filepath.Join(t.TempDir(), ".devstrap")
	root = filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init: %v (%s)", err, stderr)
	}
	ctx := context.Background()
	opts := &options{v: viper.New()}
	opts.v.Set("home", home)
	opts.v.Set("root", root)
	st, err := state.Open(ctx, opts.paths().StateDB())
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(st)

	keyring := workspacekeys.New(st, devicekeys.NewHybridStore(opts.paths().KeyDir(), platform.Detect().Keychain))
	if epoch, err := keyring.EnsureBootstrap(ctx); err != nil || epoch != 1 {
		t.Fatalf("EnsureBootstrap = %d, %v; want epoch 1", epoch, err)
	}

	remoteAge, err := devicekeys.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	remoteSigning, err := devicekeys.NewSigningIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertDevice(ctx, state.Device{
		ID: "dev_rotate_peer", Name: "peer", OS: "linux", Arch: "arm64",
		PublicKey: remoteAge.Recipient, SigningPublicKey: remoteSigning.Public, TrustState: "approved",
	}); err != nil {
		t.Fatal(err)
	}

	entry, err := st.UpsertProject(ctx, state.UpsertProjectParams{
		Path: "work/acme/api", Type: "git_repo", RemoteURL: "https://github.com/acme/api.git", RemoteKey: "github.com/acme/api",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveCapturedEnvProfile(ctx, entry.ID, "default", []string{"API_KEY"}, "age_blob:deadbeef"); err != nil {
		t.Fatal(err)
	}
	return home, root
}

func TestKeysRotateMintsAndGrantsWithoutRevokeSideEffects(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home, root := rotateTestHome(t)
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "keys", "rotate")
	if err != nil {
		t.Fatalf("keys rotate: %v (%s)", err, stderr)
	}
	if !strings.Contains(stdout, "Rotated workspace key to epoch 2") {
		t.Fatalf("stdout = %q, want rotation to epoch 2", stdout)
	}

	opts := &options{v: viper.New()}
	opts.v.Set("home", home)
	st, err := state.Open(context.Background(), opts.paths().StateDB())
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(st)
	ctx := context.Background()
	if epoch, err := st.CurrentKeyEpoch(ctx); err != nil || epoch != 2 {
		t.Fatalf("CurrentKeyEpoch = %d, %v; want 2", epoch, err)
	}
	// The grant events for the approved peer are queued as local events.
	pending, err := st.LocalPendingEvents(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	grants := 0
	for _, e := range pending {
		if e.Type == dssync.EventDeviceKeyGranted {
			grants++
		}
	}
	if grants == 0 {
		t.Fatalf("no device.key.granted events queued after rotate; pending = %d", len(pending))
	}
	// The contract pin: a pure rotation must not flag secrets for source
	// rotation (revoke semantics) — the captured binding stays unflagged —
	// and must not queue hub ciphertext deletions.
	if n, err := st.CountSecretBindingsNeedingRotation(ctx); err != nil || n != 0 {
		t.Fatalf("secret bindings flagged for rotation = %d, %v; want 0 (pure rotate must not flag secrets)", n, err)
	}
	if queued, err := st.PendingHubDeletes(ctx); err != nil || len(queued) != 0 {
		t.Fatalf("pending hub deletes = %v, %v; want none (pure rotate must not touch blobs)", queued, err)
	}
}

func TestKeysRotateRefusesAtEpochZero(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init: %v (%s)", err, stderr)
	}
	_, stderr, err := executeForTest("--home", home, "--root", root, "keys", "rotate")
	if err == nil {
		t.Fatal("keys rotate at epoch 0 succeeded, want refusal")
	}
	if !strings.Contains(stderr, "no workspace key epoch exists yet") {
		t.Fatalf("stderr = %q, want the epoch-0 refusal", stderr)
	}
}

// backdateActiveEpoch rewrites every workspace_keys created_at so
// age-triggered rotation fires (test-only surgery on the raw DB; created_at is
// stamped with the local clock in production).
func backdateActiveEpoch(t *testing.T, home string, age time.Duration) {
	t.Helper()
	opts := &options{v: viper.New()}
	opts.v.Set("home", home)
	db, err := sql.Open("sqlite", opts.paths().StateDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	backdated := time.Now().Add(-age).UTC().Format("2006-01-02T15:04:05.000000000Z")
	if _, err := db.Exec(`UPDATE workspace_keys SET created_at = ?;`, backdated); err != nil {
		t.Fatal(err)
	}
}

func TestSyncAutoRotatesStaleEpochAndPushesGrantsSameCycle(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home, root := rotateTestHome(t)
	hubPath := filepath.Join(t.TempDir(), "hub.json")
	// Prime the hub relationship: first sync pushes the existing state.
	if _, stderr, err := executeForTest("--home", home, "--root", root, "sync", "--hub-file", hubPath, "--namespace-only"); err != nil {
		t.Fatalf("first sync: %v (%s)", err, stderr)
	}
	backdateActiveEpoch(t, home, 100*24*time.Hour) // older than the 90d default

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "sync", "--hub-file", hubPath, "--namespace-only")
	if err != nil {
		t.Fatalf("rotating sync: %v (%s)", err, stderr)
	}
	if !strings.Contains(stdout, "Rotated workspace key to epoch 2") {
		t.Fatalf("stdout = %q, want the auto-rotation banner", stdout)
	}
	// The re-read assertion: the freshly minted grant events must ride THIS
	// cycle's push — the hub file already contains the epoch-2 grant.
	raw, err := os.ReadFile(hubPath)
	if err != nil {
		t.Fatal(err)
	}
	var hubEvents []state.Event
	if err := json.Unmarshal(raw, &hubEvents); err != nil {
		t.Fatalf("parse hub file: %v", err)
	}
	foundEpoch2Grant := false
	for _, e := range hubEvents {
		if e.Type != dssync.EventDeviceKeyGranted {
			continue
		}
		var grant dssync.DeviceKeyGrant
		if json.Unmarshal([]byte(e.PayloadJSON), &grant) == nil && grant.Epoch == 2 {
			foundEpoch2Grant = true
		}
	}
	if !foundEpoch2Grant {
		t.Fatalf("hub does not carry the epoch-2 grant after the rotating sync (re-read regression); %d events on hub", len(hubEvents))
	}
}

func TestSyncAutoRotateDisabledAtZero(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home, root := rotateTestHome(t)
	hubPath := filepath.Join(t.TempDir(), "hub.json")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "sync", "--hub-file", hubPath, "--namespace-only"); err != nil {
		t.Fatalf("first sync: %v (%s)", err, stderr)
	}
	backdateActiveEpoch(t, home, 100*24*time.Hour)
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "sync", "--hub-file", hubPath, "--namespace-only", "--key-max-age", "0")
	if err != nil {
		t.Fatalf("sync --key-max-age 0: %v (%s)", err, stderr)
	}
	if strings.Contains(stdout, "Rotated workspace key") {
		t.Fatalf("stdout = %q, want NO rotation with --key-max-age 0", stdout)
	}
	opts := &options{v: viper.New()}
	opts.v.Set("home", home)
	st, err := state.Open(context.Background(), opts.paths().StateDB())
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(st)
	if epoch, err := st.CurrentKeyEpoch(context.Background()); err != nil || epoch != 1 {
		t.Fatalf("CurrentKeyEpoch = %d, %v; want 1 (disabled)", epoch, err)
	}
}

func TestSyncAutoRotateSkipsKeylessDevice(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--join"); err != nil {
		t.Fatalf("init --join: %v (%s)", err, stderr)
	}
	hubPath := filepath.Join(t.TempDir(), "hub.json")
	// A keyless joiner sync (empty hub) must not rotate or found.
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "sync", "--hub-file", hubPath, "--namespace-only", "--key-max-age", "1ms")
	if err != nil {
		t.Fatalf("joiner sync: %v (%s)", err, stderr)
	}
	if strings.Contains(stdout, "Rotated workspace key") {
		t.Fatalf("stdout = %q, keyless joiner must never rotate", stdout)
	}
}

func TestSyncRejectsMalformedKeyMaxAgeFlag(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init: %v (%s)", err, stderr)
	}
	_, stderr, err := executeForTest("--home", home, "--root", root, "sync", "--hub-file", filepath.Join(t.TempDir(), "h.json"), "--key-max-age", "ninety-days")
	if err == nil {
		t.Fatal("malformed --key-max-age accepted, want usage error")
	}
	if !strings.Contains(stderr, "invalid --key-max-age") {
		t.Fatalf("stderr = %q, want the flag validation error", stderr)
	}
	// ParseDuration accepts negatives; the flag must not (post-#56 review).
	_, stderr, err = executeForTest("--home", home, "--root", root, "sync", "--hub-file", filepath.Join(t.TempDir(), "h2.json"), "--key-max-age", "-5h")
	if err == nil {
		t.Fatal("negative --key-max-age accepted, want usage error")
	}
	if !strings.Contains(stderr, "must be >= 0") {
		t.Fatalf("stderr = %q, want the negative-value refusal", stderr)
	}
}

func TestGradeWorkspaceKeyAge(t *testing.T) {
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		epoch      int64
		created    time.Time
		maxAge     time.Duration
		wantStatus checkStatus
		wantDetail string
	}{
		{"epoch zero ok", 0, time.Time{}, 2160 * time.Hour, checkOK, "no workspace key yet"},
		{"fresh ok", 3, now.Add(-24 * time.Hour), 2160 * time.Hour, checkOK, "epoch 3, age 24h"},
		{"stale warns", 2, now.Add(-2400 * time.Hour), 2160 * time.Hour, checkWarn, "exceeds keys.rotate_max_age"},
		{"disabled never warns", 2, now.Add(-9999 * time.Hour), 0, checkOK, "epoch 2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := gradeWorkspaceKeyAge(tc.epoch, tc.created, tc.maxAge, now)
			if got.Status != tc.wantStatus || !strings.Contains(got.Detail, tc.wantDetail) {
				t.Fatalf("grade = %+v, want status %s detail containing %q", got, tc.wantStatus, tc.wantDetail)
			}
		})
	}
}
