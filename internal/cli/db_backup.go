package cli

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Reederey87/DevStrap/internal/config"
	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/state"
)

// A full backup (P6-DATA-04) is a single uncompressed tar with a fixed layout;
// every entry is a regular file with mode 0600:
//
//	state.db                 VACUUM INTO snapshot of the live state database
//	config.yaml              hub pointer, root, workspace_name, role, key age
//	blobs/<sha256>.age       every age-encrypted secret referenced by the DB
//	keys/<file>              device identity, signing key, WCK epochs, hub creds
//
// The DB alone is not a recoverable backup: secrets live in blobs/ and are
// decryptable only with the private key material in keys/, and the hub pointer
// (`hub:`) plus custom `root:` live in config.yaml — without it a restored
// workspace cannot re-pull hub-synced drafts or target the right root. `db
// backup <path>` (no --full) keeps writing a bare .db; `db backup --full
// <path.tar>` writes this archive and `db restore <path.tar>` reconstructs the
// captured paths in place.
const (
	backupEntryDB       = "state.db"
	backupEntryConfig   = "config.yaml"
	backupEntryManifest = "manifest.json"
	backupDirBlobs      = "blobs"
	backupDirKeys       = "keys"
	backupEntryMode     = 0o600
	backupDirModePerm   = 0o700
)

const (
	backupManifestFormat  = "devstrap-full-backup"
	backupManifestVersion = 1
)

// backupTargets is the set of top-level paths `db backup --full` captures and
// `db restore` replaces in place. Anything else in the state dir (e.g.
// quarantine/, logs/) is neither captured nor touched by restore.
var backupTargets = []string{backupEntryDB, backupEntryConfig, backupDirBlobs, backupDirKeys}

// fullBackupResult is the machine-readable summary of a full backup.
type fullBackupResult struct {
	Path     string   `json:"path"`
	Config   bool     `json:"config"`
	Blobs    int      `json:"blobs"`
	Keys     int      `json:"keys"`
	Warnings []string `json:"warnings,omitempty"`
}

type backupManifest struct {
	Format      string                `json:"format"`
	Version     int                   `json:"version"`
	CreatedAt   string                `json:"created_at"`
	WorkspaceID string                `json:"workspace_id"`
	DeviceID    string                `json:"device_id"`
	KeyCustody  string                `json:"key_custody"`
	Entries     []backupManifestEntry `json:"entries"`
	Required    []string              `json:"required"`
}

type backupManifestEntry struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// backupSnapshotAttempts is how many VACUUM INTO + snapshot-enumerate passes
// runFullBackup will try before treating a missing referenced blob as fatal
// (P7-DATA-03). Concurrent rotation/GC can legitimately delete a superseded
// blob between attempts; a fresh snapshot will not reference it.
const backupSnapshotAttempts = 3

// backupEnumerateHook is a test seam called at the top of each
// snapshotAndEnumerate attempt (nil-checked). Tests inject concurrent drift
// (delete ciphertext, repoint bindings) between attempts.
var backupEnumerateHook func(attempt int)

// backupAfterSnapshot is a test seam for proving all metadata selection stays
// pinned to the accepted snapshot even when live state changes afterward.
var backupAfterSnapshot func()

// restoreResult is the machine-readable summary of a restore (P7-CLI-01).
type restoreResult struct {
	Restored string   `json:"restored"`
	Items    []string `json:"items"`
	Warnings []string `json:"warnings,omitempty"`
}

// runFullBackup writes a self-contained tar archive of the state database, the
// referenced encrypted blobs, and the device/workspace key material (P6-DATA-04).
// The store is already open; the DB snapshot is taken with VACUUM INTO (a
// consistent snapshot on a live WAL database — no exclusive lock).
func runFullBackup(ctx context.Context, opts *options, store *state.Store, out string, stdout io.Writer) error {
	paths := opts.paths()

	// Stage the DB snapshot in a sibling temp dir so a mid-write failure never
	// leaves a partial archive next to the requested output path.
	stageDir, err := os.MkdirTemp(filepath.Dir(out), ".devstrap-backup-")
	if err != nil {
		return appError{code: exitInvalidConfig, err: fmt.Errorf("create backup staging dir: %w", err)}
	}
	defer func() { _ = os.RemoveAll(stageDir) }()

	tmpDB := filepath.Join(stageDir, backupEntryDB)
	refs, snap, err := snapshotAndEnumerate(ctx, store, tmpDB, paths)
	if err != nil {
		return err
	}
	defer closeStore(snap)
	if backupAfterSnapshot != nil {
		backupAfterSnapshot()
	}

	keySourceDir, keyNames, err := stageBackupKeys(ctx, opts, snap, filepath.Join(stageDir, backupDirKeys))
	if err != nil {
		return err
	}
	workspaceID, err := snap.WorkspaceID(ctx)
	if err != nil {
		return err
	}
	device, err := snap.CurrentDevice(ctx)
	if err != nil {
		return err
	}
	custody, err := snap.KeyCustody(ctx)
	if err != nil {
		return err
	}
	manifest := backupManifest{
		Format:      backupManifestFormat,
		Version:     backupManifestVersion,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		WorkspaceID: workspaceID,
		DeviceID:    device.ID,
		KeyCustody:  string(custody),
		// Required is the independent recoverable core — set explicitly, NOT
		// mirrored from Entries, so the verify-side subset check is a real
		// guarantee (opus review): a crafted archive whose manifest omits the
		// device keys fails closed even before the completeness probe.
		Required: []string{
			backupEntryDB,
			path.Join(backupDirKeys, device.ID+".agekey"),
			path.Join(backupDirKeys, device.ID+".signing.key"),
		},
	}

	result, err := writeBackupTar(out, tmpDB, paths, refs, keySourceDir, keyNames, manifest)
	if err != nil {
		return err
	}

	if result.Keys == 0 {
		result.Warnings = append(result.Warnings, "no key material captured; this archive cannot decrypt secrets on its own")
	}
	if !result.Config {
		result.Warnings = append(result.Warnings, "no config.yaml found; the hub pointer and custom root will not be restored")
	}
	return opts.render(stdout, func(w io.Writer) error {
		for _, msg := range result.Warnings {
			if _, err := fmt.Fprintf(w, "warning: %s\n", msg); err != nil {
				return err
			}
		}
		_, err := fmt.Fprintf(w, "full backup written: %s (config: %t, %d blob(s), %d key file(s))\n", result.Path, result.Config, result.Blobs, result.Keys)
		return err
	}, result)
}

// snapshotAndEnumerate takes a VACUUM INTO snapshot, enumerates blob refs from
// that frozen DB file (not the live store), and verifies each ciphertext is
// present on disk. Concurrent rotation/GC may delete a superseded blob; on
// missing refs the loop retries with a fresh snapshot up to
// backupSnapshotAttempts times. Still-missing refs after the last attempt are
// a hard conflict (P7-DATA-03).
func snapshotAndEnumerate(ctx context.Context, store *state.Store, tmpDB string, paths config.Paths) ([]string, *state.Store, error) {
	blobDir := filepath.Join(paths.Home, backupDirBlobs)
	var lastMissing []string
	for attempt := 1; attempt <= backupSnapshotAttempts; attempt++ {
		if backupEnumerateHook != nil {
			backupEnumerateHook(attempt)
		}
		_ = os.Remove(tmpDB)
		// Drop any -wal/-shm left by a prior open of the snapshot file.
		_ = os.Remove(tmpDB + "-wal")
		_ = os.Remove(tmpDB + "-shm")
		if err := store.Backup(ctx, tmpDB); err != nil {
			return nil, nil, err
		}
		// P7-DATA-03 completion: keep the read-only snapshot Store open so blob
		// refs, custody, current device, and held WCK epochs all come from the
		// same frozen row-set rather than mixing snapshot and live-store reads.
		snap, err := state.OpenSnapshot(ctx, tmpDB)
		if err != nil {
			return nil, nil, err
		}
		refs, err := snap.AllBlobRefs(ctx)
		if err != nil {
			_ = snap.Close()
			return nil, nil, err
		}
		missing, err := missingBlobRefs(blobDir, refs)
		if err != nil {
			// Not an absence: permission/I-O failures are immediately fatal
			// with their real cause — retrying cannot help and reporting them
			// as "missing on disk" would misdirect the operator (CodeRabbit).
			return nil, appError{code: exitInvalidConfig, err: fmt.Errorf("full backup: inspect referenced ciphertext: %w", err)}
		}
		if len(missing) == 0 {
			return refs, snap, nil
		}
		_ = snap.Close()
		lastMissing = missing
	}
	sort.Strings(lastMissing)
	return nil, nil, appError{
		code: exitConflict,
		err: fmt.Errorf(
			"full backup: %s referenced secret ciphertext is missing on disk; run `devstrap doctor`",
			strings.Join(lastMissing, ", ")),
	}
}

// missingBlobRefs returns the refs whose ciphertext files are ABSENT (or whose
// ref is malformed). A stat failure that is not absence — permission denial,
// transient I/O — is returned as an error instead of being folded into
// "missing", so the operator sees the real cause. Same ref→filename mapping
// as writeBackupTar.
func missingBlobRefs(blobDir string, refs []string) ([]string, error) {
	var missing []string
	for _, ref := range refs {
		hash, err := envBlobHash(ref)
		if err != nil {
			missing = append(missing, ref)
			continue
		}
		if _, err := os.Stat(filepath.Join(blobDir, hash+".age")); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("stat ciphertext for %s: %w", ref, err)
			}
			missing = append(missing, ref)
		}
	}
	return missing, nil
}

// stageBackupKeys resolves this device's key material into a directory tree that
// mirrors the on-disk KeyDir layout, returning the source directory and the
// basenames to add under keys/. In every custody mode the snapshot's device,
// workspace, and exact held-epoch inventory select material from the live
// backend and escrow it into keysStage (P7-DATA-03 completion).
func stageBackupKeys(ctx context.Context, opts *options, store *state.Store, keysStage string) (string, []string, error) {
	paths := opts.paths()
	device, err := store.CurrentDevice(ctx)
	if err != nil {
		return "", nil, err
	}
	workspaceID, err := store.WorkspaceID(ctx)
	if err != nil {
		return "", nil, err
	}
	held, err := store.HeldKeys(ctx)
	if err != nil {
		return "", nil, err
	}
	epochs := make([]devicekeys.BackupEpoch, 0, len(held))
	for _, k := range held {
		epochs = append(epochs, devicekeys.BackupEpoch{Epoch: k.Epoch, KID: k.KID})
	}
	keyStore, err := resolveKeyStore(ctx, paths, store)
	if err != nil {
		return "", nil, err
	}
	names, err := keyStore.ExportForBackup(ctx, keysStage, device.ID, workspaceID, epochs)
	if err != nil {
		return "", nil, appError{code: exitInvalidConfig, err: fmt.Errorf("escrow key material for full backup: %w", err)}
	}
	return keysStage, names, nil
}

// writeBackupTar assembles the archive. A missing or unopenable referenced blob
// is a hard error: snapshotAndEnumerate already proved existence, so vanishing
// between stat and tar-write is a real failure. The deferred remove-on-error
// path cleans up any partial archive (P7-DATA-03).
func writeBackupTar(out, dbPath string, paths config.Paths, refs []string, keySourceDir string, keyNames []string, manifest backupManifest) (result fullBackupResult, err error) {
	//nolint:gosec // out is an explicit user-selected output path.
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, backupEntryMode)
	if err != nil {
		return fullBackupResult{}, appError{code: exitInvalidConfig, err: fmt.Errorf("create backup archive: %w", err)}
	}
	closed := false
	// On any failure, close and remove the partial archive so a truncated file
	// is never left behind labeled as a backup.
	defer func() {
		if !closed {
			_ = f.Close()
		}
		if err != nil {
			_ = os.Remove(out)
		}
	}()
	if err = os.Chmod(out, backupEntryMode); err != nil {
		return fullBackupResult{}, fmt.Errorf("secure backup archive: %w", err)
	}

	tw := tar.NewWriter(f)
	result = fullBackupResult{Path: out}

	add := func(name, src string) error {
		size, sum, addErr := addFileToTar(tw, name, src)
		if addErr == nil {
			manifest.Entries = append(manifest.Entries, backupManifestEntry{Name: name, Size: size, SHA256: sum})
		}
		return addErr
	}
	if err := add(backupEntryDB, dbPath); err != nil {
		return fullBackupResult{}, err
	}

	// config.yaml carries the hub pointer + custom root; capture it if present.
	// It is non-secret but still written 0600 to match the source file.
	configSrc := filepath.Join(paths.Home, backupEntryConfig)
	if _, statErr := os.Stat(configSrc); statErr == nil {
		if err := add(backupEntryConfig, configSrc); err != nil {
			return fullBackupResult{}, err
		}
		result.Config = true
	}

	blobDir := filepath.Join(paths.Home, backupDirBlobs)
	for _, ref := range refs {
		hash, herr := envBlobHash(ref)
		if herr != nil {
			return fullBackupResult{}, appError{
				code: exitConflict,
				err:  fmt.Errorf("full backup: invalid blob ref %s: %w", ref, herr),
			}
		}
		src := filepath.Join(blobDir, hash+".age")
		if err := add(path.Join(backupDirBlobs, hash+".age"), src); err != nil {
			return fullBackupResult{}, appError{
				code: exitConflict,
				err: fmt.Errorf(
					"full backup: referenced secret ciphertext for %s is unreadable (run `devstrap doctor`): %w",
					ref, err),
			}
		}
		if got := manifest.Entries[len(manifest.Entries)-1].SHA256; !strings.EqualFold(got, hash) {
			return fullBackupResult{}, appError{
				code: exitConflict,
				err:  fmt.Errorf("full backup: referenced secret ciphertext for %s does not match its content address (run `devstrap doctor`)", ref),
			}
		}
		result.Blobs++
	}

	sortedKeys := append([]string(nil), keyNames...)
	sort.Strings(sortedKeys)
	for _, name := range sortedKeys {
		if err := add(path.Join(backupDirKeys, name), filepath.Join(keySourceDir, name)); err != nil {
			return fullBackupResult{}, err
		}
		result.Keys++
	}
	manifestRaw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fullBackupResult{}, fmt.Errorf("encode backup manifest: %w", err)
	}
	manifestRaw = append(manifestRaw, '\n')
	if err := addBytesToTar(tw, backupEntryManifest, manifestRaw); err != nil {
		return fullBackupResult{}, err
	}

	if err := tw.Close(); err != nil {
		return fullBackupResult{}, fmt.Errorf("finalize backup archive: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fullBackupResult{}, fmt.Errorf("flush backup archive: %w", err)
	}
	closed = true
	if err := f.Close(); err != nil {
		return fullBackupResult{}, fmt.Errorf("close backup archive: %w", err)
	}
	return result, nil
}

// addFileToTar copies src into the archive under name with a fixed 0600 mode.
func addFileToTar(tw *tar.Writer, name, src string) (int64, string, error) {
	//nolint:gosec // src is an internally-derived path under the DevStrap home / staging dir.
	f, err := os.Open(src)
	if err != nil {
		return 0, "", fmt.Errorf("open %s for backup: %w", name, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return 0, "", fmt.Errorf("stat %s for backup: %w", name, err)
	}
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     backupEntryMode,
		Size:     info.Size(),
		Typeflag: tar.TypeReg,
	}); err != nil {
		return 0, "", fmt.Errorf("write backup header %s: %w", name, err)
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tw, h), f); err != nil {
		return 0, "", fmt.Errorf("write backup entry %s: %w", name, err)
	}
	return info.Size(), hex.EncodeToString(h.Sum(nil)), nil
}

func addBytesToTar(tw *tar.Writer, name string, contents []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: backupEntryMode, Size: int64(len(contents)), Typeflag: tar.TypeReg}); err != nil {
		return fmt.Errorf("write backup header %s: %w", name, err)
	}
	_, err := tw.Write(contents)
	return err
}

// runRestore extracts a full backup archive and replaces the captured paths
// (state.db, config.yaml, blobs/, keys/) IN PLACE in the state directory
// (P6-DATA-04). Anything else already in the state dir — quarantine/, logs/, or
// any un-captured file — is left intact; a restore is not a wipe. It refuses to
// overwrite when a captured target already exists unless force is set. Extraction
// is staged in a sibling temp dir and the DB is validated before any swap, so a
// corrupt or malicious archive never half-replaces the live state dir.
func runRestore(ctx context.Context, opts *options, in string, force, allowLegacy bool, stdout io.Writer) error {
	home := opts.paths().Home

	if !force {
		occupied, err := stateDirHasBackupTargets(home)
		if err != nil {
			return appError{code: exitInvalidConfig, err: fmt.Errorf("inspect state dir: %w", err)}
		}
		if occupied {
			return appError{code: exitConflict, err: fmt.Errorf("state dir %s is not empty (already holds restore targets); pass --force to overwrite them", home)}
		}
	}

	parent := filepath.Dir(home)
	if err := os.MkdirAll(parent, backupDirModePerm); err != nil {
		return appError{code: exitInvalidConfig, err: fmt.Errorf("create state dir parent: %w", err)}
	}
	stage, err := os.MkdirTemp(parent, ".devstrap-restore-")
	if err != nil {
		return appError{code: exitInvalidConfig, err: fmt.Errorf("create restore staging dir: %w", err)}
	}
	defer func() { _ = os.RemoveAll(stage) }()

	//nolint:gosec // in is an explicit user-selected archive path.
	f, err := os.Open(in)
	if err != nil {
		return appError{code: exitInvalidConfig, err: fmt.Errorf("open backup archive: %w", err)}
	}
	defer func() { _ = f.Close() }()
	if err := extractBackupTar(f, stage); err != nil {
		return err
	}
	legacy := false
	if _, err := os.Stat(filepath.Join(stage, backupEntryManifest)); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if !allowLegacy {
			return appError{code: exitInvalidConfig, err: fmt.Errorf("backup archive has no manifest.json (pre-P7 format); re-create it with 'devstrap db backup --full' or pass --allow-legacy to restore without integrity verification")}
		}
		legacy = true
	} else if _, err := verifyBackupManifest(stage); err != nil {
		return appError{code: exitInvalidConfig, err: fmt.Errorf("backup manifest verification failed: %w", err)}
	}

	dbPath := filepath.Join(stage, backupEntryDB)
	if _, err := os.Stat(dbPath); err != nil {
		return appError{code: exitInvalidConfig, err: fmt.Errorf("backup archive has no %s", backupEntryDB)}
	}
	if err := state.ValidateDBFileReadOnly(ctx, dbPath); err != nil {
		return appError{code: exitInvalidConfig, err: fmt.Errorf("restored database failed validation: %w", err)}
	}
	if err := verifyRestoreCompleteness(ctx, stage); err != nil {
		return err
	}

	if err := os.MkdirAll(home, backupDirModePerm); err != nil {
		return appError{code: exitInvalidConfig, err: fmt.Errorf("create state dir: %w", err)}
	}
	var restored []string
	for _, name := range backupTargets {
		swapped, err := swapBackupTarget(home, stage, name)
		if err != nil {
			return err
		}
		if swapped {
			restored = append(restored, name)
		}
	}

	result := restoreResult{Restored: home, Items: restored}
	if legacy {
		result.Warnings = append(result.Warnings, "legacy backup restored without manifest integrity verification; archive completeness was still checked against the archived database")
	}
	if msg := keychainCustodyRestoreWarning(ctx, opts); msg != "" {
		result.Warnings = append(result.Warnings, msg)
	}
	return opts.render(stdout, func(w io.Writer) error {
		for _, msg := range result.Warnings {
			if _, err := fmt.Fprintf(w, "warning: %s\n", msg); err != nil {
				return err
			}
		}
		_, err := fmt.Fprintf(w, "restored state dir: %s (%s)\n", home, strings.Join(restored, ", "))
		return err
	}, result)
}

// keychainCustodyRestoreWarning returns the custody-reconciliation guidance when
// the just-restored DB records keychain custody (P6-DATA-04), or "" otherwise.
// A --full archive lands key material as FILES, but a keychain-custody store
// reads the keychain — which is empty on a fresh machine — so without this
// warning the wedge is silent. Callers attach it to the result (P7-CLI-01).
func keychainCustodyRestoreWarning(ctx context.Context, opts *options) string {
	store, err := opts.openState(ctx)
	if err != nil {
		return ""
	}
	defer closeStore(store)
	recorded, err := store.KeyCustody(ctx)
	if err != nil {
		return ""
	}
	if recorded == devicekeys.CustodyKeychain && state.EffectiveKeyCustody(recorded) == devicekeys.CustodyKeychain {
		return fmt.Sprintf(
			"this workspace records keychain custody, but the restored key material is on disk and the keychain is empty on a fresh machine.\n"+
				"Run devstrap under %s=1 (file custody) or re-migrate the escrowed keys into the keychain before syncing.",
			platform.NoKeychainEnv)
	}
	return ""
}

// extractBackupTar unpacks an archive into dst. Every entry is validated against
// path traversal (zip-slip), restricted to the known state.db/config.yaml/blobs/
// keys layout, and written 0600; non-regular entries (dirs, symlinks) are rejected.
func extractBackupTar(r io.Reader, dst string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return appError{code: exitInvalidConfig, err: fmt.Errorf("read backup archive: %w", err)}
		}
		name, err := sanitizeBackupEntry(hdr.Name)
		if err != nil {
			return appError{code: exitInvalidConfig, err: err}
		}
		if hdr.Typeflag != tar.TypeReg {
			return appError{code: exitInvalidConfig, err: fmt.Errorf("unexpected archive entry type for %q", hdr.Name)}
		}
		target := filepath.Join(dst, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), backupDirModePerm); err != nil {
			return fmt.Errorf("create restore subdir: %w", err)
		}
		//nolint:gosec // name is zip-slip-guarded by sanitizeBackupEntry (no absolute paths, no ".." components, confined to the state.db/blobs/keys layout) and joined under the caller's staging dir.
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, backupEntryMode)
		if err != nil {
			return fmt.Errorf("create restored file %s: %w", name, err)
		}
		//nolint:gosec // hdr.Size is bounded by a local, operator-supplied archive.
		if _, err := io.Copy(out, tr); err != nil {
			_ = out.Close()
			return fmt.Errorf("write restored file %s: %w", name, err)
		}
		if err := out.Close(); err != nil {
			return fmt.Errorf("close restored file %s: %w", name, err)
		}
	}
	return nil
}

// sanitizeBackupEntry rejects unsafe archive entry names (absolute paths,
// backslashes, or any ".." component) and confines the result to the archive's
// known layout: exactly state.db, config.yaml, or manifest.json at top level,
// or a single-level file under blobs/ or keys/.
func sanitizeBackupEntry(name string) (string, error) {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, `\`) || filepath.IsAbs(name) {
		return "", fmt.Errorf("unsafe archive entry %q", name)
	}
	clean := path.Clean(name)
	parts := strings.Split(clean, "/")
	for _, p := range parts {
		if p == ".." || p == "." || p == "" {
			return "", fmt.Errorf("unsafe archive entry %q", name)
		}
	}
	switch {
	case len(parts) == 1 && (clean == backupEntryDB || clean == backupEntryConfig || clean == backupEntryManifest):
		return clean, nil
	case (strings.HasPrefix(clean, backupDirBlobs+"/") || strings.HasPrefix(clean, backupDirKeys+"/")) && len(parts) == 2:
		return clean, nil
	default:
		return "", fmt.Errorf("unexpected archive entry %q", name)
	}
}

func verifyBackupManifest(stage string) (backupManifest, error) {
	var manifest backupManifest
	//nolint:gosec // stage is a caller-created restore staging directory.
	raw, err := os.ReadFile(filepath.Join(stage, backupEntryManifest))
	if err != nil {
		return manifest, err
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return manifest, fmt.Errorf("parse manifest.json: %w", err)
	}
	if manifest.Format != backupManifestFormat || manifest.Version != backupManifestVersion {
		return manifest, fmt.Errorf("unsupported manifest format/version %q/%d", manifest.Format, manifest.Version)
	}
	listed := make(map[string]bool, len(manifest.Entries))
	for _, entry := range manifest.Entries {
		name, err := sanitizeBackupEntry(entry.Name)
		if err != nil || name == backupEntryManifest {
			return manifest, fmt.Errorf("invalid manifest entry %q", entry.Name)
		}
		if listed[name] {
			return manifest, fmt.Errorf("duplicate manifest entry %q", name)
		}
		listed[name] = true
		size, sum, err := hashFile(filepath.Join(stage, filepath.FromSlash(name)))
		if err != nil {
			return manifest, fmt.Errorf("verify %s: %w", name, err)
		}
		if size != entry.Size || !strings.EqualFold(sum, entry.SHA256) {
			return manifest, fmt.Errorf("entry %s size or SHA-256 does not match manifest", name)
		}
	}
	for _, required := range manifest.Required {
		if !listed[required] {
			return manifest, fmt.Errorf("required entry %s is not listed", required)
		}
	}
	err = filepath.WalkDir(stage, func(filePath string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(stage, filePath)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if name != backupEntryManifest && !listed[name] {
			return fmt.Errorf("unlisted archive file %s", name)
		}
		return nil
	})
	return manifest, err
}

func hashFile(name string) (int64, string, error) {
	//nolint:gosec // callers provide paths constrained to backup staging directories.
	f, err := os.Open(name)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// verifyRestoreCompleteness cross-checks recovery dependencies against the
// staged database before any live target is swapped (P7-DATA-04).
func verifyRestoreCompleteness(ctx context.Context, stage string) error {
	dbPath := filepath.Join(stage, backupEntryDB)
	snap, err := state.OpenSnapshot(ctx, dbPath)
	if err != nil {
		return appError{code: exitInvalidConfig, err: fmt.Errorf("open archived database for completeness verification: %w", err)}
	}
	defer closeStore(snap)
	missing := func(what string) error {
		return appError{code: exitInvalidConfig, err: fmt.Errorf("backup archive is incomplete: %s referenced by the archived database is missing", what)}
	}
	refs, err := snap.AllBlobRefs(ctx)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		hash, err := envBlobHash(ref)
		if err != nil {
			return missing("blob " + ref)
		}
		name := filepath.Join(stage, backupDirBlobs, hash+".age")
		_, sum, err := hashFile(name)
		if err != nil || !strings.EqualFold(sum, hash) {
			return missing("blob " + ref)
		}
	}
	device, err := snap.CurrentDevice(ctx)
	if err != nil {
		return err
	}
	// Key material must be semantically valid, not merely present (Codex
	// review): a valid-but-wrong or corrupted key file would restore
	// "successfully" into a workspace that can neither decrypt nor sign.
	// Parse the staged identities and hold their derived public halves
	// against the archived database's device row.
	unusable := func(what string, err error) error {
		return appError{code: exitInvalidConfig, err: fmt.Errorf("backup archive is unrecoverable: %s: %w", what, err)}
	}
	stagedKeys := devicekeys.NewFileStore(filepath.Join(stage, backupDirKeys))
	identity, err := stagedKeys.Read(device.ID)
	if err != nil {
		return unusable("device identity "+device.ID+".agekey does not parse", err)
	}
	if device.PublicKey != "" && identity.Recipient != device.PublicKey {
		return unusable("device identity "+device.ID+".agekey", fmt.Errorf("derived recipient does not match the archived database's device public key"))
	}
	signing, err := stagedKeys.ReadSigning(device.ID)
	if err != nil {
		return unusable("signing key "+device.ID+".signing.key does not parse", err)
	}
	if device.SigningPublicKey != "" && signing.Public != device.SigningPublicKey {
		return unusable("signing key "+device.ID+".signing.key", fmt.Errorf("derived public key does not match the archived database's device signing key"))
	}
	workspaceID, err := snap.WorkspaceID(ctx)
	if err != nil {
		return err
	}
	held, err := snap.HeldKeys(ctx)
	if err != nil {
		return err
	}
	for _, key := range held {
		name := fmt.Sprintf("wck-%s-%d.key", workspaceID, key.Epoch)
		if key.KID != "" {
			name = fmt.Sprintf("wck-%s-%d-%s.key", workspaceID, key.Epoch, key.KID)
		}
		wck, err := stagedKeys.ReadWCK(workspaceID, key.Epoch, key.KID)
		if err != nil {
			return missing("key " + name)
		}
		if len(wck) != 32 {
			return unusable("key "+name, fmt.Errorf("decoded to %d bytes, want 32", len(wck)))
		}
		if key.KID != "" {
			sum := sha256.Sum256(wck)
			if !strings.EqualFold(hex.EncodeToString(sum[:]), key.KID) {
				return unusable("key "+name, fmt.Errorf("content does not match its recorded kid fingerprint"))
			}
		}
	}
	return nil
}

// swapBackupTarget replaces home/name with stage/name when the archive carried
// it, preserving the previous copy under a .bak sibling until the rename
// succeeds and rolling back on failure. A target absent from the archive is a
// no-op. Because it swaps a single top-level path (a file or a whole subtree),
// un-captured siblings in home are never touched. Returns whether it restored.
func swapBackupTarget(home, stage, name string) (bool, error) {
	src := filepath.Join(stage, name)
	if _, err := os.Stat(src); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("inspect staged %s: %w", name, err)
	}
	dst := filepath.Join(home, name)
	aside := dst + ".bak-" + strconv.Itoa(os.Getpid())
	existed := false
	if _, err := os.Stat(dst); err == nil {
		if err := os.Rename(dst, aside); err != nil {
			return false, fmt.Errorf("move existing %s aside: %w", name, err)
		}
		existed = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("inspect existing %s: %w", name, err)
	}
	if err := os.Rename(src, dst); err != nil {
		if existed {
			_ = os.Rename(aside, dst) // best-effort rollback
		}
		return false, fmt.Errorf("promote restored %s: %w", name, err)
	}
	if existed {
		_ = os.RemoveAll(aside)
	}
	return true, nil
}

// stateDirHasBackupTargets reports whether the state dir already holds any of
// the paths a restore would replace (P6-DATA-04). Scoping the non-empty refusal
// to the captured targets means un-captured siblings (quarantine/, logs/) never
// block a restore, and a genuinely fresh state dir restores without --force.
func stateDirHasBackupTargets(home string) (bool, error) {
	for _, name := range backupTargets {
		if _, err := os.Stat(filepath.Join(home, name)); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}
