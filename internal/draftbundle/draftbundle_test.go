package draftbundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/Reederey87/DevStrap/internal/ignore"
)

// helperAgeIdentity generates an age identity and returns (identity, recipient).
func helperAgeIdentity(t *testing.T) (string, string) {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate age identity: %v", err)
	}
	return id.String(), id.Recipient().String()
}

// helperWriteFile writes a file with content under dir.
func helperWriteFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func TestPackExtractRoundTrip(t *testing.T) {
	id, recipient := helperAgeIdentity(t)
	src := t.TempDir()
	helperWriteFile(t, src, "README.md", "# hello")
	helperWriteFile(t, src, "src/main.go", "package main\n")
	helperWriteFile(t, src, "data/config.json", `{"key":"val"}`)

	snap, err := Pack(src, ignore.DefaultMatcher(), Limits{}, []string{recipient})
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	// P5-QUAL-05: FileCount counts entries (files + directories). Here: 3 files
	// (README.md, src/main.go, data/config.json) + 2 directory headers
	// (src/, data/) = 5, so empty directories survive the round-trip.
	if snap.FileCount != 5 {
		t.Errorf("entry count = %d, want 5 (3 files + 2 dirs)", snap.FileCount)
	}
	if !strings.HasPrefix(snap.BlobRef, "age_blob:") {
		t.Errorf("blob ref = %s, want age_blob: prefix", snap.BlobRef)
	}
	if len(snap.Ciphertext) == 0 {
		t.Fatal("ciphertext is empty")
	}

	dest := t.TempDir()
	if err := Extract(snap.Ciphertext, id, dest); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, rel := range []string{"README.md", "src/main.go", "data/config.json"} {
		if _, err := os.Stat(filepath.Join(dest, rel)); err != nil {
			t.Errorf("extracted file %s missing: %v", rel, err)
		}
	}
}

func TestPackRefusesSecretFiles(t *testing.T) {
	_, recipient := helperAgeIdentity(t)
	src := t.TempDir()
	helperWriteFile(t, src, ".env", "SECRET=abc")
	helperWriteFile(t, src, "app.go", "package main")

	_, err := Pack(src, ignore.DefaultMatcher(), Limits{}, []string{recipient})
	if err == nil {
		t.Fatal("Pack should refuse .env file")
	}
	if !strings.Contains(err.Error(), "secret") {
		t.Errorf("error should mention secret, got: %v", err)
	}
}

func TestPackRefusesIdRsa(t *testing.T) {
	_, recipient := helperAgeIdentity(t)
	src := t.TempDir()
	helperWriteFile(t, src, "id_rsa", "-----BEGIN PRIVATE KEY-----")
	helperWriteFile(t, src, "app.go", "package main")

	_, err := Pack(src, ignore.DefaultMatcher(), Limits{}, []string{recipient})
	if err == nil {
		t.Fatal("Pack should refuse id_rsa file")
	}
}

func TestPackEnforcesMaxBytes(t *testing.T) {
	_, recipient := helperAgeIdentity(t)
	src := t.TempDir()
	big := strings.Repeat("x", 600)
	helperWriteFile(t, src, "big.txt", big)

	_, err := Pack(src, ignore.DefaultMatcher(), Limits{MaxBytes: 500}, []string{recipient})
	if err == nil {
		t.Fatal("Pack should reject file exceeding max_bytes")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error should mention exceeds, got: %v", err)
	}
}

func TestPackEnforcesMaxFiles(t *testing.T) {
	_, recipient := helperAgeIdentity(t)
	src := t.TempDir()
	for i := range 10 {
		helperWriteFile(t, src, "f"+string(rune('0'+i))+".txt", "x")
	}

	_, err := Pack(src, ignore.DefaultMatcher(), Limits{MaxFiles: 5}, []string{recipient})
	if err == nil {
		t.Fatal("Pack should reject exceeding max_files")
	}
	if !strings.Contains(err.Error(), "max_files") {
		t.Errorf("error should mention max_files, got: %v", err)
	}
}

func TestPackRequiresRecipient(t *testing.T) {
	src := t.TempDir()
	helperWriteFile(t, src, "f.txt", "x")

	_, err := Pack(src, ignore.DefaultMatcher(), Limits{}, nil)
	if err == nil {
		t.Fatal("Pack should require at least one recipient")
	}
}

func TestExtractRejectsBadIdentity(t *testing.T) {
	_, recipient := helperAgeIdentity(t)
	src := t.TempDir()
	helperWriteFile(t, src, "f.txt", "hello")

	snap, err := Pack(src, ignore.DefaultMatcher(), Limits{}, []string{recipient})
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}

	otherID, _ := helperAgeIdentity(t)
	dest := t.TempDir()
	err = Extract(snap.Ciphertext, otherID, dest)
	if err == nil {
		t.Fatal("Extract with wrong identity should fail")
	}
}

func TestExtractDualCopyOnConflict(t *testing.T) {
	id, recipient := helperAgeIdentity(t)
	src := t.TempDir()
	helperWriteFile(t, src, "config.txt", "version=2")

	snap, err := Pack(src, ignore.DefaultMatcher(), Limits{}, []string{recipient})
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}

	dest := t.TempDir()
	helperWriteFile(t, dest, "config.txt", "version=1")

	if err := Extract(snap.Ciphertext, id, dest); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	// Original file should be preserved.
	got, err := os.ReadFile(filepath.Join(dest, "config.txt"))
	if err != nil {
		t.Fatalf("read original: %v", err)
	}
	if string(got) != "version=1" {
		t.Errorf("original = %q, want version=1", got)
	}
	// Conflict copy should have the incoming content.
	gotConflict, err := os.ReadFile(filepath.Join(dest, "config.txt.devstrap-conflict"))
	if err != nil {
		t.Fatalf("read conflict copy: %v", err)
	}
	if string(gotConflict) != "version=2" {
		t.Errorf("conflict = %q, want version=2", gotConflict)
	}
}

func TestPackIgnoresNodeModules(t *testing.T) {
	_, recipient := helperAgeIdentity(t)
	src := t.TempDir()
	helperWriteFile(t, src, "app.go", "package main")
	helperWriteFile(t, src, "node_modules/lodash/index.js", "module.exports = {}")

	snap, err := Pack(src, ignore.DefaultMatcher(), Limits{}, []string{recipient})
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if snap.FileCount != 1 {
		t.Errorf("file count = %d, want 1 (node_modules excluded)", snap.FileCount)
	}
	for _, m := range snap.Manifest {
		if strings.Contains(m, "node_modules") {
			t.Errorf("node_modules should not be in manifest, found: %s", m)
		}
	}
}

func TestPackEmptyDir(t *testing.T) {
	_, recipient := helperAgeIdentity(t)
	src := t.TempDir()

	snap, err := Pack(src, ignore.DefaultMatcher(), Limits{}, []string{recipient})
	if err != nil {
		t.Fatalf("Pack empty dir: %v", err)
	}
	if snap.FileCount != 0 {
		t.Errorf("file count = %d, want 0", snap.FileCount)
	}
}

func TestPackContentAddressedBlobRef(t *testing.T) {
	_, recipient := helperAgeIdentity(t)
	src := t.TempDir()
	helperWriteFile(t, src, "f.txt", "hello")

	snap1, err := Pack(src, ignore.DefaultMatcher(), Limits{}, []string{recipient})
	if err != nil {
		t.Fatalf("Pack 1: %v", err)
	}
	// Verify the format is correct.
	if !strings.HasPrefix(snap1.BlobRef, "age_blob:") {
		t.Errorf("blob ref format: %s", snap1.BlobRef)
	}
	if len(snap1.BlobRef) != len("age_blob:")+64 {
		t.Errorf("blob ref length = %d, want %d", len(snap1.BlobRef), len("age_blob:")+64)
	}
}

func TestExtractCreatesDirectories(t *testing.T) {
	id, recipient := helperAgeIdentity(t)
	src := t.TempDir()
	helperWriteFile(t, src, "a/b/c/deep.txt", "nested")

	snap, err := Pack(src, ignore.DefaultMatcher(), Limits{}, []string{recipient})
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}

	dest := t.TempDir()
	if err := Extract(snap.Ciphertext, id, dest); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "a/b/c/deep.txt"))
	if err != nil {
		t.Fatalf("read deep file: %v", err)
	}
	if string(got) != "nested" {
		t.Errorf("content = %q, want nested", got)
	}
}

func TestPackSkipDevstrapDir(t *testing.T) {
	_, recipient := helperAgeIdentity(t)
	src := t.TempDir()
	helperWriteFile(t, src, "app.go", "package main")
	helperWriteFile(t, src, ".devstrap/meta.json", "{}")

	snap, err := Pack(src, ignore.DefaultMatcher(), Limits{}, []string{recipient})
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if snap.FileCount != 1 {
		t.Errorf("file count = %d, want 1 (.devstrap excluded)", snap.FileCount)
	}
}

// craftBombBundle builds a raw tar→gzip→age bundle with fileCount regular
// entries of fileSize bytes each, encrypted to the test identity. It bypasses
// Pack's limits so Extract's aggregate guard (QUAL-01) can be exercised against
// a malicious bundle a compromised-but-trusted device could author.
func craftBombBundle(t *testing.T, recipient string, fileCount int, fileSize int64) []byte {
	t.Helper()
	var tarbuf bytes.Buffer
	gw := gzip.NewWriter(&tarbuf)
	tw := tar.NewWriter(gw)
	body := make([]byte, fileSize)
	for i := 0; i < fileCount; i++ {
		if err := tw.WriteHeader(&tar.Header{
			Name:     fmt.Sprintf("bomb/file%d", i),
			Mode:     0o600,
			Size:     fileSize,
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("write tar header: %v", err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("write tar body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	r, err := age.ParseX25519Recipient(recipient)
	if err != nil {
		t.Fatalf("parse recipient: %v", err)
	}
	var enc bytes.Buffer
	aw, err := age.Encrypt(&enc, r)
	if err != nil {
		t.Fatalf("age encrypt: %v", err)
	}
	if _, err := aw.Write(tarbuf.Bytes()); err != nil {
		t.Fatalf("write ciphertext: %v", err)
	}
	if err := aw.Close(); err != nil {
		t.Fatalf("close age: %v", err)
	}
	return enc.Bytes()
}

// TestExtractRejectsTooManyFiles (QUAL-01): a bundle with more entries than the
// extraction budget's file-count ceiling is aborted with ErrBundleTooLarge.
func TestExtractRejectsTooManyFiles(t *testing.T) {
	id, recipient := helperAgeIdentity(t)
	bomb := craftBombBundle(t, recipient, 10, 1)
	dest := t.TempDir()
	err := ExtractWithLimits(bomb, id, dest, Limits{MaxBytes: MaxBundleBytes, MaxFiles: 3})
	if !errors.Is(err, ErrBundleTooLarge) {
		t.Fatalf("ExtractWithLimits = %v, want ErrBundleTooLarge", err)
	}
}

// TestExtractRejectsOversizedBundle (QUAL-01): a bundle whose total
// uncompressed bytes exceed the budget is aborted (decompression bomb guard).
func TestExtractRejectsOversizedBundle(t *testing.T) {
	id, recipient := helperAgeIdentity(t)
	bomb := craftBombBundle(t, recipient, 1, 256)
	dest := t.TempDir()
	err := ExtractWithLimits(bomb, id, dest, Limits{MaxBytes: 100, MaxFiles: MaxBundleFiles})
	if !errors.Is(err, ErrBundleTooLarge) {
		t.Fatalf("ExtractWithLimits = %v, want ErrBundleTooLarge", err)
	}
}
