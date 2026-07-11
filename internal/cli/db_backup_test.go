package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
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
	var haveDB, haveConfig, haveBlob, haveKey, haveManifest bool
	var last string
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		if hdr.Mode != 0o600 {
			t.Fatalf("entry %s mode = %o, want 600", hdr.Name, hdr.Mode)
		}
		last = hdr.Name
		switch {
		case hdr.Name == "state.db":
			haveDB = true
		case hdr.Name == "config.yaml":
			haveConfig = true
		case strings.HasPrefix(hdr.Name, "blobs/") && strings.HasSuffix(hdr.Name, ".age"):
			haveBlob = true
		case strings.HasPrefix(hdr.Name, "keys/"):
			haveKey = true
		case hdr.Name == backupEntryManifest:
			haveManifest = true
		default:
			t.Fatalf("unexpected archive entry %q", hdr.Name)
		}
	}
	if !haveDB || !haveConfig || !haveBlob || !haveKey || !haveManifest || last != backupEntryManifest {
		t.Fatalf("archive missing content or manifest not last: db=%v config=%v blob=%v key=%v manifest=%v last=%q", haveDB, haveConfig, haveBlob, haveKey, haveManifest, last)
	}
}

func TestFullBackupManifestHashesEveryEntryAndIsLast(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home, root := filepath.Join(t.TempDir(), ".devstrap"), filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init: %v %s", err, stderr)
	}
	archive := filepath.Join(t.TempDir(), "backup.tar")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "db", "backup", "--full", archive); err != nil {
		t.Fatalf("backup: %v %s", err, stderr)
	}
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	files := map[string][]byte{}
	var order []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[h.Name] = body
		order = append(order, h.Name)
	}
	if order[len(order)-1] != backupEntryManifest {
		t.Fatalf("last entry=%q", order[len(order)-1])
	}
	var manifest backupManifest
	if err := json.Unmarshal(files[backupEntryManifest], &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Format != backupManifestFormat || manifest.Version != backupManifestVersion {
		t.Fatalf("manifest format/version=%q/%d", manifest.Format, manifest.Version)
	}
	if len(manifest.Entries) != len(files)-1 {
		t.Fatalf("manifest entries=%d files=%d", len(manifest.Entries), len(files))
	}
	// Required is the independently-set recoverable core (state.db + the two
	// device key files), a strict subset of Entries — never a mirror of it.
	listedNames := make(map[string]bool, len(manifest.Entries))
	for _, entry := range manifest.Entries {
		listedNames[entry.Name] = true
	}
	if len(manifest.Required) != 3 || manifest.Required[0] != backupEntryDB {
		t.Fatalf("required=%v, want state.db + the two device key files", manifest.Required)
	}
	for _, req := range manifest.Required {
		if !listedNames[req] {
			t.Fatalf("required entry %s not listed in manifest entries", req)
		}
		if req != backupEntryDB && !strings.HasPrefix(req, backupDirBlobs+"/") && !strings.HasPrefix(req, backupDirKeys+"/") {
			t.Fatalf("required entry %s is neither state.db nor a key file", req)
		}
	}
	for _, entry := range manifest.Entries {
		body, ok := files[entry.Name]
		if !ok || int64(len(body)) != entry.Size {
			t.Fatalf("entry %s missing/size mismatch", entry.Name)
		}
		sum := sha256.Sum256(body)
		if got := fmt.Sprintf("%x", sum[:]); got != entry.SHA256 {
			t.Fatalf("entry %s hash=%s want %s", entry.Name, got, entry.SHA256)
		}
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

func extractArchiveForTest(t *testing.T, archive string) string {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	stage := t.TempDir()
	if err := extractBackupTar(f, stage); err != nil {
		t.Fatal(err)
	}
	return stage
}

func repackStageForTest(t *testing.T, stage, archive string) {
	t.Helper()
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	var names []string
	if err := filepath.WalkDir(stage, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(stage, p)
		if err != nil {
			return err
		}
		names = append(names, filepath.ToSlash(rel))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(names)
	for _, name := range names {
		if name == backupEntryManifest {
			continue
		}
		if _, _, err := addFileToTar(tw, name, filepath.Join(stage, filepath.FromSlash(name))); err != nil {
			t.Fatal(err)
		}
	}
	if slices.Contains(names, backupEntryManifest) {
		if _, _, err := addFileToTar(tw, backupEntryManifest, filepath.Join(stage, backupEntryManifest)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func newFullBackupForTest(t *testing.T) (string, string, string) {
	t.Helper()
	t.Setenv(platform.NoKeychainEnv, "1")
	home, root := filepath.Join(t.TempDir(), ".devstrap"), filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init: %v %s", err, stderr)
	}
	registerEnvProject(t, home, root, "tamper-secret")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "env", "capture", "work/proj", ".env"); err != nil {
		t.Fatalf("capture: %v %s", err, stderr)
	}
	archive := filepath.Join(t.TempDir(), "backup.tar")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "db", "backup", "--full", archive); err != nil {
		t.Fatalf("backup: %v %s", err, stderr)
	}
	return home, root, archive
}

func TestRestoreRejectsTamperedEntriesBeforeSwap(t *testing.T) {
	for _, tc := range []string{"blob", "key", "extra"} {
		t.Run(tc, func(t *testing.T) {
			_, root, archive := newFullBackupForTest(t)
			stage := extractArchiveForTest(t, archive)
			var target string
			if tc == "extra" {
				target = filepath.Join(stage, backupDirKeys, "extra.key")
				if err := os.WriteFile(target, []byte("extra"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				dir := map[string]string{"blob": backupDirBlobs, "key": backupDirKeys}[tc]
				entries, err := os.ReadDir(filepath.Join(stage, dir))
				if err != nil || len(entries) == 0 {
					t.Fatalf("entries: %v %v", entries, err)
				}
				target = filepath.Join(stage, dir, entries[0].Name())
				raw, err := os.ReadFile(target)
				if err != nil {
					t.Fatal(err)
				}
				if tc == "blob" {
					raw[0] ^= 0xff
				} else {
					raw = raw[:len(raw)/2]
				}
				if err := os.WriteFile(target, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			tampered := filepath.Join(t.TempDir(), "tampered.tar")
			repackStageForTest(t, stage, tampered)
			restoreHome := filepath.Join(t.TempDir(), "restore")
			if err := os.MkdirAll(restoreHome, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(restoreHome, backupEntryConfig), []byte("workspace_name: live-before\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, stderr, err := executeForTest("--home", restoreHome, "--root", root, "db", "restore", tampered, "--force")
			if err == nil || !strings.Contains(stderr, "manifest") {
				t.Fatalf("restore err=%v stderr=%q", err, stderr)
			}
			if got, _ := os.ReadFile(filepath.Join(restoreHome, backupEntryConfig)); string(got) != "workspace_name: live-before\n" {
				t.Fatalf("live target changed: %q", got)
			}
		})
	}
}

func TestRestoreRejectsMissingOrShortArchiveBeforeSwap(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Code")
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{name: "empty"},
		{name: "short-tar-header", body: bytes.Repeat([]byte{'x'}, 100)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			archive := filepath.Join(t.TempDir(), "broken.tar")
			if err := os.WriteFile(archive, tc.body, 0o600); err != nil {
				t.Fatal(err)
			}
			restoreHome := filepath.Join(t.TempDir(), "restore")
			if err := os.MkdirAll(restoreHome, 0o700); err != nil {
				t.Fatal(err)
			}
			live := filepath.Join(restoreHome, backupEntryConfig)
			if err := os.WriteFile(live, []byte("workspace_name: live-before\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := executeForTest("--home", restoreHome, "--root", root, "db", "restore", archive, "--force"); err == nil {
				t.Fatal("restore accepted an incomplete archive")
			}
			if got, err := os.ReadFile(live); err != nil || string(got) != "workspace_name: live-before\n" {
				t.Fatalf("live target changed: %q err=%v", got, err)
			}
		})
	}
}

func TestRestoreLegacyPolicyAndCompleteness(t *testing.T) {
	_, root, archive := newFullBackupForTest(t)
	stage := extractArchiveForTest(t, archive)
	if err := os.Remove(filepath.Join(stage, backupEntryManifest)); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(t.TempDir(), "legacy.tar")
	repackStageForTest(t, stage, legacy)
	restoreHome := filepath.Join(t.TempDir(), "restore")
	if _, stderr, err := executeForTest("--home", restoreHome, "--root", root, "db", "restore", legacy); err == nil || !strings.Contains(stderr, "--allow-legacy") {
		t.Fatalf("legacy refusal err=%v stderr=%q", err, stderr)
	}
	stdout, stderr, err := executeForTest("--home", restoreHome, "--root", root, "db", "restore", legacy, "--allow-legacy")
	if err != nil || !strings.Contains(stdout, "without manifest integrity verification") {
		t.Fatalf("legacy restore err=%v stdout=%q stderr=%q", err, stdout, stderr)
	}

	incompleteStage := extractArchiveForTest(t, archive)
	blobs, err := os.ReadDir(filepath.Join(incompleteStage, backupDirBlobs))
	if err != nil || len(blobs) == 0 {
		t.Fatalf("blobs=%v err=%v", blobs, err)
	}
	missingName := path.Join(backupDirBlobs, blobs[0].Name())
	if err := os.Remove(filepath.Join(incompleteStage, filepath.FromSlash(missingName))); err != nil {
		t.Fatal(err)
	}
	removeManifestEntryForTest(t, incompleteStage, missingName)
	incomplete := filepath.Join(t.TempDir(), "incomplete.tar")
	repackStageForTest(t, incompleteStage, incomplete)
	if _, stderr, err := executeForTest("--home", filepath.Join(t.TempDir(), "incomplete-home"), "--root", root, "db", "restore", incomplete); err == nil || !strings.Contains(stderr, "archive is incomplete") {
		t.Fatalf("incomplete restore err=%v stderr=%q", err, stderr)
	}
}

func removeManifestEntryForTest(t *testing.T, stage, name string) {
	t.Helper()
	manifestPath := filepath.Join(stage, backupEntryManifest)
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest backupManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Entries = slices.DeleteFunc(manifest.Entries, func(e backupManifestEntry) bool { return e.Name == name })
	manifest.Required = slices.DeleteFunc(manifest.Required, func(required string) bool { return required == name })
	raw, err = json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreCompletenessRequiresHeldWCKFile(t *testing.T) {
	home, root, archive := newFullBackupForTest(t)
	st, err := state.Open(t.Context(), filepath.Join(home, backupEntryDB))
	if err != nil {
		t.Fatal(err)
	}
	workspaceID, err := st.WorkspaceID(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordKeyEpoch(t.Context(), 1, "", "self"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := (devicekeys.FileStore{Dir: filepath.Join(home, backupDirKeys)}).WriteWCK(workspaceID, 1, "", bytes.Repeat([]byte{7}, 32)); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "db", "backup", "--full", archive); err != nil {
		t.Fatalf("backup after rotate: %v %s", err, stderr)
	}
	stage := extractArchiveForTest(t, archive)
	keys, err := os.ReadDir(filepath.Join(stage, backupDirKeys))
	if err != nil {
		t.Fatal(err)
	}
	missingName := ""
	for _, key := range keys {
		if strings.HasPrefix(key.Name(), "wck-") {
			missingName = path.Join(backupDirKeys, key.Name())
			break
		}
	}
	if missingName == "" {
		t.Fatal("backup contained no held WCK file")
	}
	if err := os.Remove(filepath.Join(stage, filepath.FromSlash(missingName))); err != nil {
		t.Fatal(err)
	}
	removeManifestEntryForTest(t, stage, missingName)
	incomplete := filepath.Join(t.TempDir(), "missing-wck.tar")
	repackStageForTest(t, stage, incomplete)
	_, stderr, err := executeForTest("--home", filepath.Join(t.TempDir(), "restore"), "--root", root, "db", "restore", incomplete)
	if err == nil || !strings.Contains(stderr, "archive is incomplete") || !strings.Contains(stderr, "wck-") {
		t.Fatalf("restore err=%v stderr=%q", err, stderr)
	}
}

// TestRestoreRefusesSemanticallyInvalidKeyMaterial (Codex review): key files
// that PARSE but do not match the archived database's device row — or a WCK
// whose bytes contradict its kid fingerprint — must refuse the restore, not
// merely files that are absent. Exercised via the legacy (manifest-less) path
// because a tampered file would otherwise be caught earlier by the manifest
// hash check.
func TestRestoreRefusesSemanticallyInvalidKeyMaterial(t *testing.T) {
	home, root, archive := newFullBackupForTest(t)
	if _, stderr, err := executeForTest("--home", home, "--root", root, "db", "backup", "--full", archive); err != nil {
		t.Fatalf("backup: %v %s", err, stderr)
	}
	stage := extractArchiveForTest(t, archive)
	// Replace the device identity with a DIFFERENT valid age key: parses fine,
	// derived recipient no longer matches the archived database.
	keys, err := os.ReadDir(filepath.Join(stage, backupDirKeys))
	if err != nil {
		t.Fatal(err)
	}
	ageName := ""
	for _, k := range keys {
		if strings.HasSuffix(k.Name(), ".agekey") {
			ageName = k.Name()
			break
		}
	}
	if ageName == "" {
		t.Fatal("no staged agekey")
	}
	wrong, err := devicekeys.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, backupDirKeys, ageName), []byte(wrong.Private+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(stage, backupEntryManifest)); err != nil {
		t.Fatal(err)
	}
	tampered := filepath.Join(t.TempDir(), "wrong-key.tar")
	repackStageForTest(t, stage, tampered)
	_, stderr, err := executeForTest("--home", filepath.Join(t.TempDir(), "restore"), "--root", root, "db", "restore", tampered, "--allow-legacy")
	if err == nil || !strings.Contains(stderr, "does not match the archived database") {
		t.Fatalf("restore err=%v stderr=%q", err, stderr)
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

// TestFullBackupJSONWarningsInPayload proves that under --json, non-fatal
// warnings ride the payload's warnings array and the entire stdout is a single
// parseable JSON document (P7-CLI-01). Missing blobs are fatal (P7-DATA-03);
// this covers the remaining config/key warning path.
func TestFullBackupJSONWarningsInPayload(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")

	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init: %v stderr=%s", err, stderr)
	}
	// Remove config.yaml so the backup warns about the missing hub pointer
	// without failing (config is optional; blobs/keys are not).
	if err := os.Remove(filepath.Join(home, "config.yaml")); err != nil {
		t.Fatal(err)
	}

	archive := filepath.Join(t.TempDir(), "workspace.tar")
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json", "db", "backup", "--full", archive)
	if err != nil {
		t.Fatalf("db backup --full --json: %v stderr=%s stdout=%s", err, stderr, stdout)
	}
	if strings.Contains(stdout, "warning:") {
		t.Fatalf("stdout must not contain raw warning: text outside JSON: %s", stdout)
	}

	var got fullBackupResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("full stdout is not a single JSON document: %v\n%s", err, stdout)
	}
	if got.Config {
		t.Fatalf("expected config=false after deleting config.yaml, got %+v", got)
	}
	found := false
	for _, w := range got.Warnings {
		if strings.Contains(w, "no config.yaml") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("warnings = %v, want one mentioning no config.yaml", got.Warnings)
	}
}

// TestFullBackupMissingBlobFatal proves a referenced blob whose ciphertext is
// gone is a hard error: no archive is left at the output path and no staging
// dir lingers (P7-DATA-03).
func TestFullBackupMissingBlobFatal(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	outDir := t.TempDir()

	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init: %v stderr=%s", err, stderr)
	}
	registerEnvProject(t, home, root, "top-secret")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "env", "capture", "work/proj", ".env"); err != nil {
		t.Fatalf("env capture: %v stderr=%s", err, stderr)
	}

	ctx := context.Background()
	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	refs, err := store.AllBlobRefs(ctx)
	_ = store.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) == 0 {
		t.Fatal("expected at least one blob ref after capture")
	}
	missingRef := refs[0]

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

	archive := filepath.Join(outDir, "workspace.tar")
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "db", "backup", "--full", archive)
	if err == nil {
		t.Fatalf("expected full backup to fail with missing blob; stdout=%s", stdout)
	}
	errText := err.Error() + stderr
	if !strings.Contains(errText, missingRef) {
		t.Fatalf("error must name the missing ref %q; err=%v stderr=%s", missingRef, err, stderr)
	}
	if !strings.Contains(errText, "referenced secret ciphertext is missing on disk") {
		t.Fatalf("error must mention missing ciphertext; err=%v stderr=%s", err, stderr)
	}
	if _, statErr := os.Stat(archive); !os.IsNotExist(statErr) {
		t.Fatalf("archive must not exist after failed backup: %v", statErr)
	}
	// No stray staging dirs next to the output.
	leftovers, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range leftovers {
		if strings.HasPrefix(e.Name(), ".devstrap-backup-") {
			t.Fatalf("stray backup staging dir left behind: %s", e.Name())
		}
	}
}

func TestFullBackupRejectsCorruptContentAddressedBlob(t *testing.T) {
	home, root, _ := newFullBackupForTest(t)
	entries, err := os.ReadDir(filepath.Join(home, backupDirBlobs))
	if err != nil || len(entries) == 0 {
		t.Fatalf("blobs=%v err=%v", entries, err)
	}
	if err := os.WriteFile(filepath.Join(home, backupDirBlobs, entries[0].Name()), []byte("corrupt ciphertext"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "corrupt.tar")
	_, stderr, err := executeForTest("--home", home, "--root", root, "db", "backup", "--full", archive)
	if err == nil || !strings.Contains(stderr, "does not match its content address") {
		t.Fatalf("backup err=%v stderr=%q", err, stderr)
	}
	if _, err := os.Stat(archive); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial archive remains: %v", err)
	}
}

func TestFullBackupFailsWhenSnapshotHeldWCKDisappearsFromLiveCustody(t *testing.T) {
	home, root, _ := newFullBackupForTest(t)
	st, err := state.Open(t.Context(), filepath.Join(home, backupEntryDB))
	if err != nil {
		t.Fatal(err)
	}
	workspaceID, err := st.WorkspaceID(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordKeyEpoch(t.Context(), 1, "", "self"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	wckPath := filepath.Join(home, backupDirKeys, fmt.Sprintf("wck-%s-1.key", workspaceID))
	if err := (devicekeys.FileStore{Dir: filepath.Join(home, backupDirKeys)}).WriteWCK(workspaceID, 1, "", bytes.Repeat([]byte{9}, 32)); err != nil {
		t.Fatal(err)
	}
	backupAfterSnapshot = func() {
		if err := os.Remove(wckPath); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { backupAfterSnapshot = nil })
	archive := filepath.Join(t.TempDir(), "missing-wck.tar")
	_, stderr, err := executeForTest("--home", home, "--root", root, "db", "backup", "--full", archive)
	if err == nil || !strings.Contains(stderr, "escrow key material") {
		t.Fatalf("backup err=%v stderr=%q", err, stderr)
	}
	if _, err := os.Stat(archive); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("archive remains: %v", err)
	}
}

// TestFullBackupRetriesOnDrift injects concurrent rotation drift on attempt 1
// (delete ciphertext + repoint the live binding) so the first snapshot misses a
// blob; consistency is restored before attempt 2 and the backup succeeds
// (P7-DATA-03).
func TestFullBackupRetriesOnDrift(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")

	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init: %v stderr=%s", err, stderr)
	}
	registerEnvProject(t, home, root, "rotate-me")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "env", "capture", "work/proj", ".env"); err != nil {
		t.Fatalf("env capture: %v stderr=%s", err, stderr)
	}

	ctx := context.Background()
	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	refs, err := store.AllBlobRefs(ctx)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if len(refs) != 1 {
		_ = store.Close()
		t.Fatalf("refs = %v, want exactly one after capture", refs)
	}
	oldRef := refs[0]
	hash, err := envBlobHash(oldRef)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	oldBlobPath := filepath.Join(home, "blobs", hash+".age")
	_ = store.Close()

	// New content-addressed blob representing a completed rotation. The file
	// is written only when consistency is restored (attempt 2), so attempt 1
	// sees a snapshot that references a missing ciphertext.
	rotatedCiphertext := []byte("rotated ciphertext")
	newSum := sha256.Sum256(rotatedCiphertext)
	newHash := fmt.Sprintf("%x", newSum[:])
	newRef := "age_blob:" + newHash
	newBlobPath := filepath.Join(home, "blobs", newHash+".age")

	var attempts []int
	backupEnumerateHook = func(attempt int) {
		attempts = append(attempts, attempt)
		switch attempt {
		case 1:
			// Simulate rotation mid-backup: old ciphertext gone, live binding
			// already repointed to the new ref whose file is not yet durable.
			_ = os.Remove(oldBlobPath)
			st, err := state.Open(ctx, filepath.Join(home, "state.db"))
			if err != nil {
				t.Errorf("hook open store: %v", err)
				return
			}
			if err := st.UpdateBlobRef(ctx, oldRef, newRef); err != nil {
				t.Errorf("hook UpdateBlobRef: %v", err)
			}
			_ = st.Close()
		case 2:
			// Restore consistency: rotated ciphertext is now on disk.
			if err := os.WriteFile(newBlobPath, rotatedCiphertext, 0o600); err != nil {
				t.Errorf("hook write new blob: %v", err)
			}
		}
	}
	t.Cleanup(func() { backupEnumerateHook = nil })

	archive := filepath.Join(t.TempDir(), "workspace.tar")
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "db", "backup", "--full", archive)
	if err != nil {
		t.Fatalf("db backup --full after drift: %v stderr=%s stdout=%s", err, stderr, stdout)
	}
	if len(attempts) < 2 {
		t.Fatalf("hook saw attempts %v, want at least 2", attempts)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("archive missing after successful retry: %v", err)
	}
	assertBackupTarRefsMatchDB(t, archive)
}

// assertBackupTarRefsMatchDB opens the archive's state.db and checks every
// blob ref it holds has a matching blobs/<sha>.age entry in the tar.
func assertBackupTarRefsMatchDB(t *testing.T, archive string) {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	tr := tar.NewReader(f)
	blobEntries := map[string]bool{}
	var dbBytes []byte
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		switch {
		case hdr.Name == "state.db":
			dbBytes, err = io.ReadAll(tr)
			if err != nil {
				t.Fatalf("read state.db from tar: %v", err)
			}
		case strings.HasPrefix(hdr.Name, "blobs/") && strings.HasSuffix(hdr.Name, ".age"):
			blobEntries[hdr.Name] = true
		}
	}
	if len(dbBytes) == 0 {
		t.Fatal("archive missing state.db")
	}
	tmp := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(tmp, dbBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := state.OpenSnapshot(context.Background(), tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	refs, err := snap.AllBlobRefs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		hash, err := envBlobHash(ref)
		if err != nil {
			t.Fatalf("ref %s: %v", ref, err)
		}
		want := "blobs/" + hash + ".age"
		if !blobEntries[want] {
			t.Fatalf("archive state.db references %s but tar has no %s (have %v)", ref, want, blobEntries)
		}
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
