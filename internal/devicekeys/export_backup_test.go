package devicekeys

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestExportForBackupEscrowsKeychainMaterial proves the P6-DATA-04 escrow: a
// keychain-custody store keeps no key material on disk, but ExportForBackup
// reads the device identities and held WCK epochs out of the keychain and
// writes them to a directory in the FileStore on-disk format, so the escrow
// files are complete and independent of the keychain backend.
func TestExportForBackupEscrowsKeychainMaterial(t *testing.T) {
	ctx := context.Background()
	backend := &memorySecretBackend{}
	store := NewHybridStore(t.TempDir(), backend).WithCustody(CustodyKeychain)
	const deviceID = "dev_escrow"
	const workspaceID = "ws_escrow"

	id, _, err := store.Ensure(ctx, deviceID, "")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	sig, _, err := store.EnsureSigning(ctx, deviceID, "")
	if err != nil {
		t.Fatalf("EnsureSigning: %v", err)
	}
	wck := make([]byte, 32)
	for i := range wck {
		wck[i] = byte(i)
	}
	if err := store.StoreWCK(ctx, workspaceID, 1, "", wck); err != nil {
		t.Fatalf("StoreWCK: %v", err)
	}

	// Keychain custody must not have written anything to the KeyDir.
	if entries, _ := os.ReadDir(store.File.Dir); len(entries) != 0 {
		t.Fatalf("keychain custody wrote %d KeyDir files, want 0", len(entries))
	}

	dst := t.TempDir()
	written, err := store.ExportForBackup(ctx, dst, deviceID, workspaceID, []BackupEpoch{{Epoch: 1, KID: ""}})
	if err != nil {
		t.Fatalf("ExportForBackup: %v", err)
	}
	wantNames := []string{deviceID + ".agekey", deviceID + ".signing.key", "wck-" + workspaceID + "-1.key"}
	sort.Strings(wantNames)
	if strings.Join(written, ",") != strings.Join(wantNames, ",") {
		t.Fatalf("written = %v, want %v", written, wantNames)
	}

	// Every escrowed file must be 0600.
	for _, name := range written {
		info, err := os.Stat(filepath.Join(dst, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v, want 0600", name, info.Mode().Perm())
		}
	}

	// A file-only store must reconstruct the identical material from the escrow,
	// with no keychain backend in play.
	fileStore := NewFileStore(dst)
	gotID, err := fileStore.Read(deviceID)
	if err != nil {
		t.Fatalf("read escrowed age identity: %v", err)
	}
	if gotID.Recipient != id.Recipient || gotID.Private != id.Private {
		t.Fatal("escrowed age identity does not match the original")
	}
	gotSig, err := fileStore.ReadSigning(deviceID)
	if err != nil {
		t.Fatalf("read escrowed signing identity: %v", err)
	}
	if gotSig.Public != sig.Public {
		t.Fatal("escrowed signing identity does not match the original")
	}
	gotWCK, err := fileStore.ReadWCK(workspaceID, 1, "")
	if err != nil {
		t.Fatalf("read escrowed WCK: %v", err)
	}
	if !bytes.Equal(gotWCK, wck) {
		t.Fatal("escrowed WCK does not match the original")
	}
}

// TestExportForBackupFailsLoudlyOnMissingWCK proves a full backup refuses to
// silently drop key material: a held epoch whose WCK cannot be read is a hard,
// named error (P6-DATA-04).
func TestExportForBackupFailsLoudlyOnMissingWCK(t *testing.T) {
	ctx := context.Background()
	store := NewHybridStore(t.TempDir(), &memorySecretBackend{}).WithCustody(CustodyKeychain)
	const deviceID = "dev_escrow"
	const workspaceID = "ws_escrow"
	if _, _, err := store.Ensure(ctx, deviceID, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.EnsureSigning(ctx, deviceID, ""); err != nil {
		t.Fatal(err)
	}
	// Epoch 7 was never stored, so escrowing it must fail.
	_, err := store.ExportForBackup(ctx, t.TempDir(), deviceID, workspaceID, []BackupEpoch{{Epoch: 7, KID: ""}})
	if err == nil {
		t.Fatal("expected an error escrowing a missing WCK epoch")
	}
	if !strings.Contains(err.Error(), "workspace key epoch 7") {
		t.Fatalf("error = %v, want it to name the missing epoch", err)
	}
}

// TestExportForBackupSkipsPresentFiles proves ExportForBackup does not overwrite
// a file the caller already staged (e.g. a KeyDir copy).
func TestExportForBackupSkipsPresentFiles(t *testing.T) {
	ctx := context.Background()
	store := NewHybridStore(t.TempDir(), &memorySecretBackend{}).WithCustody(CustodyKeychain)
	const deviceID = "dev_escrow"
	const workspaceID = "ws_escrow"
	if _, _, err := store.Ensure(ctx, deviceID, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.EnsureSigning(ctx, deviceID, ""); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	// Pre-stage the age identity file with sentinel content.
	sentinel := []byte("PRE-STAGED\n")
	if err := os.WriteFile(filepath.Join(dst, deviceID+".agekey"), sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	written, err := store.ExportForBackup(ctx, dst, deviceID, workspaceID, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The pre-staged agekey must not be reported as written or overwritten.
	for _, name := range written {
		if name == deviceID+".agekey" {
			t.Fatal("ExportForBackup overwrote a pre-staged file")
		}
	}
	got, err := os.ReadFile(filepath.Join(dst, deviceID+".agekey"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, sentinel) {
		t.Fatal("pre-staged agekey content changed")
	}
}
