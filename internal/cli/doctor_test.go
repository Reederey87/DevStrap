package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
	"github.com/spf13/viper"
)

func TestDoctorErrorsOnOpenHubHashChainBreak(t *testing.T) {
	env, store, _ := setupRecovery(t, true)
	defer closeStore(store)
	if err := store.InsertConflict(env.ctx, "", dssync.ConflictEventHashChain, `{"device_id":"dev_lost"}`); err != nil {
		t.Fatal(err)
	}
	results := checkHubHashChainConflicts(env.ctx, store)
	if len(results) != 1 || results[0].Status != checkError || !strings.Contains(results[0].Detail, "possible hub data loss") {
		t.Fatalf("hash-chain doctor result = %+v, want data-loss error", results)
	}
}

func TestDoctorWarnsOnHashChainHoldExplainedByPendingGrant(t *testing.T) {
	env, store, _ := setupRecovery(t, true)
	defer closeStore(store)
	if _, err := store.NoteMissingKeyGrant(env.ctx, 2, "kid-missing"); err != nil {
		t.Fatal(err)
	}
	carrier := state.Event{
		ID:          "evt_predecessor",
		WorkspaceID: env.wsID,
		DeviceID:    "dev_offline",
		Seq:         4,
		HLC:         4,
		Type:        dssync.EventEncryptedV2,
		PayloadJSON: `{"v":2,"epoch":2,"kid":"kid-missing","ct":"preserved"}`,
	}
	rawCarrier, err := json.Marshal(carrier)
	if err != nil {
		t.Fatal(err)
	}
	rawVerification, err := json.Marshal(map[string]any{
		"kind":       dssync.EventConflictKindUndecryptable,
		"event_id":   carrier.ID,
		"device_id":  carrier.DeviceID,
		"event_json": string(rawCarrier),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertConflict(env.ctx, "", dssync.ConflictEventVerification, string(rawVerification)); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertConflict(env.ctx, "", dssync.ConflictEventHashChain, `{"event_id":"evt_successor","device_id":"dev_offline","seq":5}`); err != nil {
		t.Fatal(err)
	}
	results := checkHubHashChainConflicts(env.ctx, store)
	if len(results) != 1 || results[0].Status != checkWarn || !strings.Contains(results[0].Detail, "explained by pending workspace key grant") {
		t.Fatalf("hash-chain doctor result = %+v, want pending-grant warning", results)
	}
	if strings.Contains(results[0].Detail, "possible hub data loss") {
		t.Fatalf("pending-grant hold was mislabeled as data loss: %+v", results[0])
	}
}

func TestDoctorDurabilityExportStalenessIsOptIn(t *testing.T) {
	env, store, _ := setupRecovery(t, true)
	defer closeStore(store)
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	results := checkDurabilityExport(env.ctx, env.opts, store, now)
	if len(results) != 1 || results[0].Status != checkOK || !strings.Contains(results[0].Detail, "not configured") {
		t.Fatalf("unconfigured durability result = %+v, want informational ok", results)
	}

	env.opts.v.Set("hub_replica", "file:/replica/hub.json")
	env.opts.v.Set(durabilityExportConfigKey, "24h")
	results = checkDurabilityExport(env.ctx, env.opts, store, now)
	if results[0].Status != checkWarn || !strings.Contains(results[0].Detail, "no successful export") {
		t.Fatalf("never-exported durability result = %+v, want warning", results)
	}
	record := durabilityExportRecord{
		Replica: "file:/replica/hub.json", SnapshotSHA256: strings.Repeat("a", 64), ExportedAt: now.Add(-49 * time.Hour),
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetLocalMeta(env.ctx, durabilityExportMetaKey, string(raw)); err != nil {
		t.Fatal(err)
	}
	results = checkDurabilityExport(env.ctx, env.opts, store, now)
	if results[0].Status != checkWarn || !strings.Contains(results[0].Detail, "49h") {
		t.Fatalf("stale durability result = %+v, want age warning", results)
	}
}

func TestShouldWarnWorkspaceIDMismatch(t *testing.T) {
	tests := []struct {
		name       string
		role       string
		hubID      string
		pullCursor int64
		hasEvents  bool
		want       bool
	}{
		{
			name:       "joiner r2 cursor zero no events",
			role:       "joiner",
			hubID:      "r2:ws_test",
			pullCursor: 0,
			hasEvents:  false,
			want:       true,
		},
		{
			name:       "founder r2 cursor zero no events",
			role:       "founder",
			hubID:      "r2:ws_test",
			pullCursor: 0,
			hasEvents:  false,
			want:       false,
		},
		{
			name:       "joiner file cursor zero no events",
			role:       "joiner",
			hubID:      "file:/tmp/hub.json",
			pullCursor: 0,
			hasEvents:  false,
			want:       false,
		},
		{
			name:       "joiner r2 cursor advanced no events",
			role:       "joiner",
			hubID:      "r2:ws_test",
			pullCursor: 100,
			hasEvents:  false,
			want:       false,
		},
		{
			name:       "joiner r2 cursor zero has events",
			role:       "joiner",
			hubID:      "r2:ws_test",
			pullCursor: 0,
			hasEvents:  true,
			want:       false,
		},
		{
			name:       "joiner s3 cursor zero no events",
			role:       " joiner ",
			hubID:      "s3:ws_test",
			pullCursor: 0,
			hasEvents:  false,
			want:       true,
		},
		{
			name:       "joiner git cursor zero no events",
			role:       "joiner",
			hubID:      "git:ws_test",
			pullCursor: 0,
			hasEvents:  false,
			want:       true,
		},
		{
			name:       "founder git cursor zero no events",
			role:       "founder",
			hubID:      "git:ws_test",
			pullCursor: 0,
			hasEvents:  false,
			want:       false,
		},
		{
			name:       "joiner git cursor advanced no events",
			role:       "joiner",
			hubID:      "git:ws_test",
			pullCursor: 100,
			hasEvents:  false,
			want:       false,
		},
		{
			name:       "joiner folder cursor zero no events",
			role:       "joiner",
			hubID:      "folder:ws_test",
			pullCursor: 0,
			hasEvents:  false,
			want:       true,
		},
		{
			name:       "founder folder cursor zero no events",
			role:       "founder",
			hubID:      "folder:ws_test",
			pullCursor: 0,
			hasEvents:  false,
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldWarnWorkspaceIDMismatch(tt.role, tt.hubID, tt.pullCursor, tt.hasEvents)
			if got != tt.want {
				t.Fatalf("shouldWarnWorkspaceIDMismatch(%q, %q, %d, %v) = %v, want %v", tt.role, tt.hubID, tt.pullCursor, tt.hasEvents, got, tt.want)
			}
		})
	}
}

func TestCheckHubHealthWorkspaceIDRowFileHub(t *testing.T) {
	ctx := context.Background()
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--join"); err != nil {
		t.Fatalf("init --join stderr = %q err = %v", stderr, err)
	}

	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	wsID, err := store.WorkspaceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	closeStore(store)

	v := viper.New()
	v.Set("home", home)
	v.Set("root", root)
	v.Set("role", "joiner")
	opts := &options{v: v}

	results := checkHubHealth(ctx, opts, filepath.Join(t.TempDir(), "hub.json"))
	var foundWorkspaceID bool
	for _, result := range results {
		if result.Name == "workspace id" {
			foundWorkspaceID = true
			if result.Status != checkOK || result.Detail != wsID {
				t.Fatalf("workspace id row = %+v, want ok detail %q", result, wsID)
			}
		}
		if result.Name == "workspace id match" {
			t.Fatalf("file-backed hub emitted workspace id match warning: %+v", result)
		}
	}
	if !foundWorkspaceID {
		t.Fatalf("checkHubHealth results = %+v, want workspace id row", results)
	}
}

// TestCheckHubHealthWarnsOnRetentionManifestVersionSkew pins the P7-PROD-03
// doctor surface: a retention manifest behind what this binary produces is a
// WARNING (a live signal of a mixed-version fleet), never a failure —
// `doctor --remote` must not wedge on it.
func TestCheckHubHealthWarnsOnRetentionManifestVersionSkew(t *testing.T) {
	ctx := context.Background()
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--join"); err != nil {
		t.Fatalf("init --join stderr = %q err = %v", stderr, err)
	}
	v := viper.New()
	v.Set("home", home)
	v.Set("root", root)
	opts := &options{v: v}

	hubPath := filepath.Join(t.TempDir(), "hub.json")
	old := dssync.RetentionManifest{
		V:           1,
		WorkspaceID: "ws_test",
		Floors:      map[string]int64{"dev_a": 5},
		Snapshot: dssync.RetentionSnapshotRef{
			Epoch: 1, HLC: 1, KID: "kid", ProducedBy: "dev_a", SHA256: strings.Repeat("a", 64),
		},
		ProducedBy: "dev_a",
		ProducedAt: 1,
		Sig:        "not-verified-by-this-diagnostic",
	}
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := (dssync.FileHub{Path: hubPath}).PutRetention(ctx, raw, ""); err != nil {
		t.Fatal(err)
	}

	results := checkHubHealth(ctx, opts, hubPath)
	var found bool
	for _, result := range results {
		if result.Name == "retention manifest version" {
			found = true
			if result.Status != checkWarn || !strings.Contains(result.Detail, "v1") || !strings.Contains(result.Detail, "v2") {
				t.Fatalf("retention manifest version result = %+v, want warning mentioning v1/v2", result)
			}
		}
	}
	if !found {
		t.Fatalf("checkHubHealth results = %+v, want retention manifest version warning", results)
	}
}

// TestCheckHubHealthWarnsWhenManifestRequiresNewerReader covers the other
// skew direction (P7-PROD-03): a manifest that stamps a MinReaderVersion
// above what this binary reads is a warning telling the operator to upgrade
// THIS device, distinct from the "hub is behind" message above.
func TestCheckHubHealthWarnsWhenManifestRequiresNewerReader(t *testing.T) {
	ctx := context.Background()
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--join"); err != nil {
		t.Fatalf("init --join stderr = %q err = %v", stderr, err)
	}
	v := viper.New()
	v.Set("home", home)
	v.Set("root", root)
	opts := &options{v: v}

	hubPath := filepath.Join(t.TempDir(), "hub.json")
	future := dssync.RetentionManifest{
		V:           dssync.CurrentSnapshotVersion(),
		WorkspaceID: "ws_test",
		Floors:      map[string]int64{"dev_a": 5},
		Snapshot: dssync.RetentionSnapshotRef{
			Epoch: 1, HLC: 1, KID: "kid", ProducedBy: "dev_a", SHA256: strings.Repeat("a", 64),
		},
		ProducedBy:       "dev_a",
		ProducedAt:       1,
		MinReaderVersion: dssync.CurrentSnapshotVersion() + 1,
		Sig:              "not-verified-by-this-diagnostic",
	}
	raw, err := json.Marshal(future)
	if err != nil {
		t.Fatal(err)
	}
	if err := (dssync.FileHub{Path: hubPath}).PutRetention(ctx, raw, ""); err != nil {
		t.Fatal(err)
	}

	results := checkHubHealth(ctx, opts, hubPath)
	var found bool
	for _, result := range results {
		if result.Name == "retention manifest version" {
			found = true
			if result.Status != checkWarn || !strings.Contains(result.Detail, "min reader version") {
				t.Fatalf("retention manifest version result = %+v, want warning mentioning min reader version", result)
			}
		}
	}
	if !found {
		t.Fatalf("checkHubHealth results = %+v, want retention manifest version warning", results)
	}
}

func TestDoctorWarnsWhenServiceInstalledButStopped(t *testing.T) {
	f := &fakeServiceManager{
		labelVal:  "fake.run-loop",
		statusVal: platform.ServiceStatus{Installed: true, Running: false, Detail: "not loaded", UnitPath: "/x/fake.plist"},
	}
	withFakeService(t, f)

	v := viper.New()
	v.Set("home", t.TempDir())
	opts := &options{v: v}

	results := checkService(context.Background(), opts, nil)
	if len(results) != 1 {
		t.Fatalf("checkService results = %+v, want exactly one", results)
	}
	got := results[0]
	if got.Name != "run-loop service" || got.Status != checkWarn {
		t.Fatalf("service check = %+v, want a warning row", got)
	}
	if !strings.Contains(got.Remedy, "journalctl --user -u fake.run-loop") {
		t.Errorf("remedy = %q, want the inspection hint", got.Remedy)
	}
}

func TestDoctorWarnsWhenServiceExecPathMissing(t *testing.T) {
	f := &fakeServiceManager{statusVal: platform.ServiceStatus{
		Installed:       true,
		Running:         false,
		ExecPath:        "/opt/homebrew/Cellar/devstrap/old/bin/devstrap",
		ExecPathMissing: true,
	}}
	withFakeService(t, f)

	v := viper.New()
	v.Set("home", t.TempDir())
	results := checkService(context.Background(), &options{v: v}, nil)
	if len(results) != 1 || results[0].Status != checkWarn {
		t.Fatalf("checkService results = %+v, want one warning", results)
	}
	if !strings.Contains(results[0].Detail, f.statusVal.ExecPath) {
		t.Errorf("detail = %q, want missing path", results[0].Detail)
	}
	wantRemedy := "re-run devstrap service install (the installed unit points at a binary that no longer exists — e.g. after a brew upgrade)"
	if results[0].Remedy != wantRemedy {
		t.Errorf("remedy = %q, want %q", results[0].Remedy, wantRemedy)
	}
}

func TestDoctorWarnsForInstalledServiceWithKeychainCustody(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "")
	f := &fakeServiceManager{nameVal: "systemd-user", statusVal: platform.ServiceStatus{Installed: true, Running: true}}
	withFakeService(t, f)
	home := serviceTestHomeWithCustody(t, devicekeys.CustodyKeychain)
	store, err := state.Open(t.Context(), filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)

	results := checkService(t.Context(), &options{v: viper.New()}, store)
	for _, result := range results {
		if result.Name == "run-loop service custody" {
			if result.Status != checkWarn || !strings.Contains(result.Detail, "no session D-Bus") || !strings.Contains(result.Remedy, platform.NoKeychainEnv) {
				t.Fatalf("custody result = %+v, want systemd keychain warning and remedy", result)
			}
			return
		}
	}
	t.Fatalf("results = %+v, want run-loop service custody warning", results)
}

func TestDoctorDoesNotWarnForInstalledServiceWithFileCustody(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "")
	f := &fakeServiceManager{nameVal: "systemd-user", statusVal: platform.ServiceStatus{Installed: true, Running: true}}
	withFakeService(t, f)
	home := serviceTestHomeWithCustody(t, devicekeys.CustodyFile)
	store, err := state.Open(t.Context(), filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)

	results := checkService(t.Context(), &options{v: viper.New()}, store)
	for _, result := range results {
		if result.Name == "run-loop service custody" {
			t.Fatalf("results = %+v, want no custody warning for file custody", results)
		}
	}
}

func TestDoctorThreadsCustodyStoreIntoServiceCheck(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "")
	f := &fakeServiceManager{nameVal: "systemd-user", statusVal: platform.ServiceStatus{Installed: true, Running: true}}
	withFakeService(t, f)
	home := serviceTestHomeWithCustody(t, devicekeys.CustodyKeychain)
	v := viper.New()
	v.Set("home", home)
	v.Set("root", filepath.Join(home, "root"))

	results := runDoctorChecks(t.Context(), &options{v: v})
	for _, result := range results {
		if result.Name == "run-loop service custody" {
			return
		}
	}
	t.Fatalf("results = %+v, want custody warning from the command-level doctor checks", results)
}
