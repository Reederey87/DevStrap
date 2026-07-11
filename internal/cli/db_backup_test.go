package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/state"
)

// registerEnvProject inserts a git_repo project and materializes its local
// directory with a .env file so `env capture` can run. It is shared by the
// backup round-trip tests.
func registerEnvProject(t *testing.T, home, root, secret string) string {
	t.Helper()
	ctx := context.Background()
	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertProject(ctx, state.UpsertProjectParams{
		Path:      "work/proj",
		Type:      "git_repo",
		RemoteKey: "github.com/acme/proj",
		RemoteURL: "https://github.com/acme/proj",
	}); err != nil {
		closeStore(store)
		t.Fatal(err)
	}
	closeStore(store)

	projDir := filepath.Join(root, "work", "proj")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, ".env"), []byte("API_KEY="+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return projDir
}

// TestFullBackupRestoreRoundTrip is the P6-DATA-04 key test: capture a secret,
// take a --full backup, wipe the whole state dir, restore, and prove `env
// hydrate` recovers the identical plaintext — which is only possible if the
// archive carried the encrypted blob AND the device key material, not just the
// database. Run under file custody so the round-trip is hermetic.
func TestFullBackupRestoreRoundTrip(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")

	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init: %v stderr=%s", err, stderr)
	}
	const secret = "secret-xyz-42"
	projDir := registerEnvProject(t, home, root, secret)

	// Put a non-default hub pointer in config.yaml so the round-trip proves it
	// is captured and restored (without it a restored workspace cannot re-pull
	// hub-synced drafts).
	const hubLine = "hub: \"r2://roundtrip-bucket\""
	appendConfigLine(t, home, hubLine)

	if stdout, stderr, err := executeForTest("--home", home, "--root", root, "env", "capture", "work/proj", ".env"); err != nil {
		t.Fatalf("env capture: %v stderr=%s stdout=%s", err, stderr, stdout)
	}

	archive := filepath.Join(t.TempDir(), "workspace.tar")
	if stdout, stderr, err := executeForTest("--home", home, "--root", root, "db", "backup", "--full", archive); err != nil {
		t.Fatalf("db backup --full: %v stderr=%s stdout=%s", err, stderr, stdout)
	} else if !strings.Contains(stdout, "config: true") {
		t.Fatalf("backup did not report config capture: %s", stdout)
	}
	if info, err := os.Stat(archive); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("archive mode = %v, want 0600", info.Mode().Perm())
	}
	assertBackupTarLayout(t, archive)

	// Wipe the entire state dir — DB, blobs, and keys all gone.
	if err := os.RemoveAll(home); err != nil {
		t.Fatal(err)
	}

	if stdout, stderr, err := executeForTest("--home", home, "--root", root, "db", "restore", archive); err != nil {
		t.Fatalf("db restore: %v stderr=%s stdout=%s", err, stderr, stdout)
	}

	// config.yaml (hub pointer + root) must be restored.
	cfg, err := os.ReadFile(filepath.Join(home, "config.yaml"))
	if err != nil {
		t.Fatalf("config.yaml not restored: %v", err)
	}
	if !strings.Contains(string(cfg), hubLine) {
		t.Fatalf("restored config.yaml missing hub pointer: %s", cfg)
	}
	if !strings.Contains(string(cfg), "root:") {
		t.Fatalf("restored config.yaml missing root: %s", cfg)
	}

	if stdout, stderr, err := executeForTest("--home", home, "--root", root, "env", "hydrate", "work/proj", "--write", ".env.local"); err != nil {
		t.Fatalf("env hydrate: %v stderr=%s stdout=%s", err, stderr, stdout)
	}
	got, err := os.ReadFile(filepath.Join(projDir, ".env.local"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), secret) {
		t.Fatalf("hydrated env %q does not contain the original secret", string(got))
	}
}

// appendConfigLine appends a line to the state dir's config.yaml (0600).
func appendConfigLine(t *testing.T, home, line string) {
	t.Helper()
	path := filepath.Join(home, "config.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	if err := os.WriteFile(path, append(raw, []byte(line+"\n")...), 0o600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
}

// assertBackupTarLayout checks the archive holds state.db, at least one blob,
// and the key material, every entry 0600 and confined to the known layout.
func assertBackupTarLayout(t *testing.T, archive string) {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	tr := tar.NewReader(f)
	var haveDB, haveConfig, haveBlob, haveKey bool
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		if hdr.Mode != 0o600 {
			t.Fatalf("entry %s mode = %o, want 600", hdr.Name, hdr.Mode)
		}
		switch {
		case hdr.Name == "state.db":
			haveDB = true
		case hdr.Name == "config.yaml":
			haveConfig = true
		case strings.HasPrefix(hdr.Name, "blobs/") && strings.HasSuffix(hdr.Name, ".age"):
			haveBlob = true
		case strings.HasPrefix(hdr.Name, "keys/"):
			haveKey = true
		default:
			t.Fatalf("unexpected archive entry %q", hdr.Name)
		}
	}
	if !haveDB || !haveConfig || !haveBlob || !haveKey {
		t.Fatalf("archive missing content: db=%v config=%v blob=%v key=%v", haveDB, haveConfig, haveBlob, haveKey)
	}
}

// TestRestoreRefusesNonEmptyStateDir proves restore will not clobber a live
// state dir without --force, and succeeds with it (P6-DATA-04).
func TestRestoreRefusesNonEmptyStateDir(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")

	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init: %v stderr=%s", err, stderr)
	}
	archive := filepath.Join(t.TempDir(), "workspace.tar")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "db", "backup", "--full", archive); err != nil {
		t.Fatalf("db backup --full: %v stderr=%s", err, stderr)
	}

	// An un-captured file under Home must survive a --force restore (restore
	// replaces only the captured targets, it does not wipe the state dir).
	quarantine := filepath.Join(home, "quarantine", "2026-07-04", "keep.txt")
	if err := os.MkdirAll(filepath.Dir(quarantine), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(quarantine, []byte("do-not-delete"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The state dir is non-empty (init populated it): restore must refuse.
	_, stderr, err := executeForTest("--home", home, "--root", root, "db", "restore", archive)
	if err == nil {
		t.Fatal("expected restore to refuse a non-empty state dir")
	}
	if !strings.Contains(stderr, "not empty") {
		t.Fatalf("stderr = %q, want a not-empty refusal", stderr)
	}

	// --force overwrites the captured targets.
	if _, stderr, err := executeForTest("--home", home, "--root", root, "db", "restore", archive, "--force"); err != nil {
		t.Fatalf("db restore --force: %v stderr=%s", err, stderr)
	}
	if _, err := os.Stat(filepath.Join(home, "state.db")); err != nil {
		t.Fatalf("state.db missing after forced restore: %v", err)
	}
	// The un-captured quarantine file must still be there.
	if got, err := os.ReadFile(quarantine); err != nil {
		t.Fatalf("un-captured quarantine file destroyed by restore: %v", err)
	} else if string(got) != "do-not-delete" {
		t.Fatalf("quarantine file content changed: %q", got)
	}
}

// TestExtractBackupTarRejectsZipSlip is the zip-slip regression: a crafted entry
// escaping the destination (or otherwise outside the known layout) is rejected
// and nothing is written outside the staging directory (P6-DATA-04).
func TestExtractBackupTarRejectsZipSlip(t *testing.T) {
	cases := []struct {
		name  string
		entry string
	}{
		{"parent-escape", "../escape.txt"},
		{"nested-parent-escape", "keys/../../escape.txt"},
		{"absolute", "/etc/escape.txt"},
		{"unexpected-root", "evil.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			body := []byte("pwned")
			if err := tw.WriteHeader(&tar.Header{Name: tc.entry, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write(body); err != nil {
				t.Fatal(err)
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}

			dst := filepath.Join(t.TempDir(), "stage")
			if err := os.MkdirAll(dst, 0o700); err != nil {
				t.Fatal(err)
			}
			err := extractBackupTar(bytes.NewReader(buf.Bytes()), dst)
			if err == nil {
				t.Fatalf("extract accepted unsafe entry %q", tc.entry)
			}
			if !strings.Contains(err.Error(), "archive entry") {
				t.Fatalf("err = %v, want an archive-entry rejection", err)
			}
			// Nothing must have escaped next to the staging dir.
			if _, statErr := os.Stat(filepath.Join(filepath.Dir(dst), "escape.txt")); statErr == nil {
				t.Fatal("zip-slip wrote a file outside the destination")
			}
		})
	}
}

// TestExtractBackupTarRejectsSymlink proves a symlink entry is rejected (only
// regular files are accepted).
func TestExtractBackupTarRejectsSymlink(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "keys/link", Mode: 0o600, Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := extractBackupTar(bytes.NewReader(buf.Bytes()), dst); err == nil {
		t.Fatal("expected symlink entry to be rejected")
	}
}

// TestFullBackupJSONWarningsInPayload proves that under --json, missing-blob
// warnings are carried in the payload's warnings array and the entire stdout is
// a single parseable JSON document (P7-CLI-01).
func TestFullBackupJSONWarningsInPayload(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")

	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init: %v stderr=%s", err, stderr)
	}
	registerEnvProject(t, home, root, "top-secret")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "env", "capture", "work/proj", ".env"); err != nil {
		t.Fatalf("env capture: %v stderr=%s", err, stderr)
	}

	// Delete the referenced blob so the full backup must warn and omit it.
	blobDir := filepath.Join(home, "blobs")
	entries, err := os.ReadDir(blobDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.Remove(filepath.Join(blobDir, e.Name())); err != nil {
			t.Fatal(err)
		}
	}

	archive := filepath.Join(t.TempDir(), "workspace.tar")
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json", "db", "backup", "--full", archive)
	if err != nil {
		t.Fatalf("db backup --full --json: %v stderr=%s stdout=%s", err, stderr, stdout)
	}

	var got fullBackupResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("full stdout is not a single JSON document: %v\n%s", err, stdout)
	}
	if len(got.Warnings) == 0 {
		t.Fatalf("expected non-empty warnings, got %+v", got)
	}
	found := false
	for _, w := range got.Warnings {
		if strings.Contains(w, "referenced blob(s) missing") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("warnings = %v, want one mentioning referenced blob(s) missing", got.Warnings)
	}
}

// TestRestoreJSONIsSingleDocument proves that under --json, restore emits one
// JSON document for the entire stdout with no leading "warning:" text (P7-CLI-01).
func TestRestoreJSONIsSingleDocument(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")

	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init: %v stderr=%s", err, stderr)
	}
	archive := filepath.Join(t.TempDir(), "workspace.tar")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "db", "backup", "--full", archive); err != nil {
		t.Fatalf("db backup --full: %v stderr=%s", err, stderr)
	}

	// Restore into a fresh home so no --force is required.
	restoreHome := filepath.Join(t.TempDir(), ".devstrap-restore")
	stdout, stderr, err := executeForTest("--home", restoreHome, "--root", root, "--json", "db", "restore", archive)
	if err != nil {
		t.Fatalf("db restore --json: %v stderr=%s stdout=%s", err, stderr, stdout)
	}
	if strings.Contains(stdout, "warning:") {
		t.Fatalf("stdout must not contain raw warning: text outside JSON: %s", stdout)
	}

	var got restoreResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("full stdout is not a single JSON document: %v\n%s", err, stdout)
	}
	if got.Restored != restoreHome {
		t.Fatalf("restored = %q, want %q", got.Restored, restoreHome)
	}
	if len(got.Items) == 0 {
		t.Fatalf("expected non-empty items, got %+v", got)
	}
}

// TestRestoreJSONCarriesKeychainCustodyWarning proves the custody guidance —
// the warning that originally corrupted the --json stream — rides the payload's
// warnings array instead of preceding it (P7-CLI-01).
func TestRestoreJSONCarriesKeychainCustodyWarning(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")

	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init: %v stderr=%s", err, stderr)
	}
	// Flip the recorded custody to keychain so the restored DB reports it
	// (RecordKeyCustody is write-once and init already recorded file custody).
	// The backup itself still runs under DEVSTRAP_NO_KEYCHAIN=1 (effective file
	// custody), so key material is captured from the KeyDir and the real
	// keychain is never touched.
	ctx := context.Background()
	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetLocalMeta(ctx, "key_custody", string(devicekeys.CustodyKeychain)); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	_ = store.Close()

	archive := filepath.Join(t.TempDir(), "workspace.tar")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "db", "backup", "--full", archive); err != nil {
		t.Fatalf("db backup --full: %v stderr=%s", err, stderr)
	}

	// Un-force file custody for the restore so EffectiveKeyCustody(keychain)
	// stays keychain and the guidance fires.
	t.Setenv(platform.NoKeychainEnv, "0")
	restoreHome := filepath.Join(t.TempDir(), ".devstrap-restore")
	stdout, stderr, err := executeForTest("--home", restoreHome, "--root", root, "--json", "db", "restore", archive)
	if err != nil {
		t.Fatalf("db restore --json: %v stderr=%s stdout=%s", err, stderr, stdout)
	}
	if strings.Contains(stdout, "warning:") {
		t.Fatalf("stdout must not contain raw warning: text outside JSON: %s", stdout)
	}
	var got restoreResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("full stdout is not a single JSON document: %v\n%s", err, stdout)
	}
	found := false
	for _, w := range got.Warnings {
		if strings.Contains(w, "keychain custody") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("warnings = %v, want the keychain-custody guidance", got.Warnings)
	}
}

// TestDoctorFlagsDanglingBlobRef proves doctor grades a referenced-but-missing
// blob as an error (P6-DATA-04): exactly the wreckage a DB-only restore leaves.
func TestDoctorFlagsDanglingBlobRef(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")

	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init: %v stderr=%s", err, stderr)
	}
	registerEnvProject(t, home, root, "top-secret")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "env", "capture", "work/proj", ".env"); err != nil {
		t.Fatalf("env capture: %v stderr=%s", err, stderr)
	}

	// Healthy first: no dangling refs.
	if stdout, _, err := executeForTest("--home", home, "--root", root, "doctor"); err != nil {
		t.Fatalf("doctor (healthy): %v stdout=%s", err, stdout)
	} else if !strings.Contains(stdout, "blob refs") {
		t.Fatalf("doctor output missing blob-refs check: %s", stdout)
	}

	// Delete the referenced blob to simulate a DB-only restore.
	blobDir := filepath.Join(home, "blobs")
	entries, err := os.ReadDir(blobDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.Remove(filepath.Join(blobDir, e.Name())); err != nil {
			t.Fatal(err)
		}
	}

	stdout, _, err := executeForTest("--home", home, "--root", root, "doctor")
	if err == nil {
		t.Fatal("expected doctor to fail with a dangling blob ref")
	}
	if !strings.Contains(stdout, "dangling blob refs") {
		t.Fatalf("doctor output missing dangling blob refs finding: %s", stdout)
	}
}
