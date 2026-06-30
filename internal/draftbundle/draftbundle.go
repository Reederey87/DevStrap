// Package draftbundle packs and unpacks non-git project content as
// age-encrypted, content-addressed blobs (DRAFT-02). The bundle format is:
//
//	walk project dir under the .devstrapignore allow-list (DRAFT-03) and the
//	draft_projects size/file-count limits (DRAFT-04) → tar → gzip →
//	age-encrypt to approved-device recipients → one content-addressed
//	age_blob:<sha256> blob.
//
// node_modules and build artifacts are never bundled (DRAFT-05): they are
// excluded by the ignore compiler and rebuilt on hydrate. Plaintext secret
// files / private keys are never bundled: the shared secret detector refuses
// them before encryption.
//
// Extraction is the inverse: age-decrypt → gunzip → untar into the skeleton.
// Opaque drafts are dual-copy-on-conflict, never byte-merged (decision #7).
package draftbundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
	"github.com/Reederey87/DevStrap/internal/ignore"
)

// MaxBundleBytes bounds a single draft bundle so a runaway draft cannot produce
// a giant encrypted blob. Per-project overrides live in draft_projects.
const MaxBundleBytes = 100 * 1024 * 1024 // 100 MiB

// MaxBundleFiles is the default file-count ceiling for both Pack and Extract
// (DRAFT-04 / QUAL-01).
const MaxBundleFiles = 5000

// ErrBundleTooLarge signals that an extraction exceeded the aggregate
// decompression budget (QUAL-01), aborting a gzip/tar bomb authored by a
// compromised-but-trusted device.
var ErrBundleTooLarge = errors.New("draft bundle exceeds extraction budget (decompression bomb guard)")

// Limits are the enforced size and file-count bounds for a draft bundle
// (DRAFT-04).
type Limits struct {
	MaxBytes int64
	MaxFiles int64
}

// Snapshot records the metadata of a packed draft bundle.
type Snapshot struct {
	BlobRef  string // age_blob:<sha256>
	ByteSize int64
	// FileCount is the number of tar entries (regular files + directory
	// headers) in the bundle (P5-QUAL-05). Counting directory entries keeps the
	// extract-side decompression-bomb budget (P5-SEC-02) and the materialize
	// floor symmetric with what Pack produced.
	FileCount  int64
	Manifest   []string // relative paths included, for diagnostics
	Ciphertext []byte   // the encrypted blob (caller stores it)
}

// Pack walks dir under the compiled ignore policy and the given limits, tars +
// gzips + age-encrypts the content to recipients, and returns the
// content-addressed snapshot (DRAFT-02). Secret-looking files are refused
// before encryption.
func Pack(dir string, matcher *ignore.Matcher, limits Limits, recipients []string) (Snapshot, error) {
	if len(recipients) == 0 {
		return Snapshot{}, fmt.Errorf("at least one age recipient is required")
	}
	if limits.MaxBytes <= 0 {
		limits.MaxBytes = MaxBundleBytes
	}
	if limits.MaxFiles <= 0 {
		limits.MaxFiles = MaxBundleFiles
	}
	ageRecipients := make([]age.Recipient, 0, len(recipients))
	for _, raw := range recipients {
		r, err := age.ParseX25519Recipient(raw)
		if err != nil {
			return Snapshot{}, fmt.Errorf("parse age recipient: %w", err)
		}
		ageRecipients = append(ageRecipients, r)
	}
	var tarbuf bytes.Buffer
	gw := gzip.NewWriter(&tarbuf)
	tw := tar.NewWriter(gw)
	var totalBytes int64
	var fileCount int64
	var manifest []string
	cleanDir, err := filepath.Abs(filepath.Clean(dir))
	if err != nil {
		return Snapshot{}, fmt.Errorf("resolve draft dir: %w", err)
	}
	err = filepath.WalkDir(cleanDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == cleanDir {
			return nil
		}
		rel, err := filepath.Rel(cleanDir, path)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		if d.IsDir() {
			if matcher != nil && matcher.ShouldPruneDir(d.Name(), relSlash) {
				return filepath.SkipDir
			}
			// Skip the .devstrap metadata dir inside drafts.
			if relSlash == ".devstrap" {
				return filepath.SkipDir
			}
			// P5-QUAL-05: emit an explicit directory header so empty
			// directories (placeholder logs/, tmp/, scaffold dirs) survive the
			// cross-device round-trip. Count it against MaxFiles so the
			// decompression-bomb guard (P5-SEC-02) stays sound.
			info, err := d.Info()
			if err != nil {
				return fmt.Errorf("stat dir %s: %w", relSlash, err)
			}
			dhdr := &tar.Header{
				Name:     relSlash + "/",
				Mode:     0o750,
				ModTime:  info.ModTime(),
				Typeflag: tar.TypeDir,
			}
			if err := tw.WriteHeader(dhdr); err != nil {
				return fmt.Errorf("write tar dir header %s: %w", relSlash, err)
			}
			fileCount++
			if fileCount > limits.MaxFiles {
				return fmt.Errorf("draft bundle exceeds max_files %d; raise draft_projects.max_files or add content to .devstrapignore", limits.MaxFiles)
			}
			return nil
		}
		if matcher != nil && matcher.Match(relSlash, false) {
			return nil
		}
		if isSecretPath(relSlash) {
			return fmt.Errorf("refusing to bundle secret-looking file %s; add it to .devstrapignore or remove it", relSlash)
		}
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("stat %s: %w", relSlash, err)
		}
		if info.Size() > limits.MaxBytes {
			return fmt.Errorf("file %s is %d bytes, exceeds draft limit %d; raise draft_projects.max_bytes or add to .devstrapignore", relSlash, info.Size(), limits.MaxBytes)
		}
		//nolint:gosec // The path is an explicit user project file bounded by the limits above.
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", relSlash, err)
		}
		defer func() { _ = f.Close() }()
		hdr := &tar.Header{
			Name:     relSlash,
			Mode:     0o600,
			Size:     info.Size(),
			ModTime:  info.ModTime(),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("write tar header %s: %w", relSlash, err)
		}
		n, err := io.Copy(tw, io.LimitReader(f, limits.MaxBytes-totalBytes+1))
		if err != nil {
			return fmt.Errorf("copy %s into bundle: %w", relSlash, err)
		}
		totalBytes += n
		fileCount++
		manifest = append(manifest, relSlash)
		if totalBytes > limits.MaxBytes {
			return fmt.Errorf("draft bundle exceeds max_bytes %d; raise draft_projects.max_bytes or add content to .devstrapignore", limits.MaxBytes)
		}
		if fileCount > limits.MaxFiles {
			return fmt.Errorf("draft bundle exceeds max_files %d; raise draft_projects.max_files or add content to .devstrapignore", limits.MaxFiles)
		}
		return nil
	})
	if err != nil {
		return Snapshot{}, err
	}
	if err := tw.Close(); err != nil {
		return Snapshot{}, fmt.Errorf("close tar: %w", err)
	}
	if err := gw.Close(); err != nil {
		return Snapshot{}, fmt.Errorf("close gzip: %w", err)
	}
	// Age-encrypt the compressed tar.
	var enc bytes.Buffer
	aw, err := age.Encrypt(&enc, ageRecipients...)
	if err != nil {
		return Snapshot{}, fmt.Errorf("encrypt draft bundle: %w", err)
	}
	if _, err := aw.Write(tarbuf.Bytes()); err != nil {
		return Snapshot{}, fmt.Errorf("write draft bundle: %w", err)
	}
	if err := aw.Close(); err != nil {
		return Snapshot{}, fmt.Errorf("close draft bundle: %w", err)
	}
	ciphertext := enc.Bytes()
	sum := sha256.Sum256(ciphertext)
	return Snapshot{
		BlobRef:    "age_blob:" + hex.EncodeToString(sum[:]),
		ByteSize:   totalBytes,
		FileCount:  fileCount,
		Manifest:   manifest,
		Ciphertext: ciphertext,
	}, nil
}

// Extract age-decrypts the bundle with the local device identity, then
// gunzips and untars it into dest (DRAFT-02 hydrate) with a default aggregate
// extraction budget (QUAL-01): at most MaxBundleBytes total uncompressed bytes
// and MaxBundleFiles files, aborting a gzip/tar decompression bomb authored by
// a compromised-but-trusted device. Files are written 0600; directories 0750.
// Existing files are not overwritten (dual-copy conflict safety, decision #7).
func Extract(ciphertext []byte, identity, dest string) error {
	return ExtractWithLimits(ciphertext, identity, dest, Limits{MaxBytes: MaxBundleBytes, MaxFiles: MaxBundleFiles})
}

// ExtractWithLimits is like Extract but with a caller-supplied aggregate
// extraction budget (QUAL-01). The running total uncompressed bytes and entry
// count are tracked across the whole tar stream — not just per file — so a
// bomb that spreads across many small entries or one huge entry is caught.
func ExtractWithLimits(ciphertext []byte, identity, dest string, limits Limits) error {
	if limits.MaxBytes <= 0 {
		limits.MaxBytes = MaxBundleBytes
	}
	if limits.MaxFiles <= 0 {
		limits.MaxFiles = MaxBundleFiles
	}
	ageIdentity, err := age.ParseX25519Identity(identity)
	if err != nil {
		return fmt.Errorf("parse age identity: %w", err)
	}
	reader, err := age.Decrypt(bytes.NewReader(ciphertext), ageIdentity)
	if err != nil {
		return fmt.Errorf("decrypt draft bundle: %w", err)
	}
	gr, err := gzip.NewReader(reader)
	if err != nil {
		return fmt.Errorf("read draft bundle gzip: %w", err)
	}
	defer func() { _ = gr.Close() }()
	tr := tar.NewReader(gr)
	cleanDest, err := filepath.Abs(filepath.Clean(dest))
	if err != nil {
		return fmt.Errorf("resolve extract dest: %w", err)
	}
	if err := os.MkdirAll(cleanDest, 0o750); err != nil {
		return fmt.Errorf("create extract dest: %w", err)
	}
	var totalBytes int64
	var fileCount int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar entry: %w", err)
		}
		if strings.ContainsAny(hdr.Name, `\\`) || filepath.IsAbs(hdr.Name) {
			return fmt.Errorf("refusing tar path outside extract root: %s", hdr.Name)
		}
		target := filepath.Join(cleanDest, filepath.FromSlash(hdr.Name))
		if !pathWithin(cleanDest, target) {
			return fmt.Errorf("refusing tar path outside extract root: %s", hdr.Name)
		}
		// P5-SEC-02: reject any entry type Pack never produces. Pack emits only
		// regular files (TypeReg) and directories (TypeDir); anything else
		// (symlink, device, fifo, GNU/PAX extension records that can carry data)
		// is attacker-introduced and must not slip past the budget guard.
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeDir {
			return fmt.Errorf("unsupported tar entry %q (type %d)", hdr.Name, hdr.Typeflag)
		}
		// P5-SEC-02 / QUAL-01: charge the aggregate decompression budget for
		// EVERY entry, before the type switch, so a crafted header with a large
		// declared Size cannot walk past the guard regardless of its type. A
		// zero-size entry passes even when the budget is exactly exhausted
		// (0 > 0 is false), matching Pack's strict-`>` accounting.
		fileCount++
		if fileCount > limits.MaxFiles {
			return fmt.Errorf("%w: %d files exceeds limit %d", ErrBundleTooLarge, fileCount, limits.MaxFiles)
		}
		if hdr.Size > 0 {
			remaining := limits.MaxBytes - totalBytes
			if hdr.Size > remaining {
				return fmt.Errorf("%w: entry %s (%d bytes) exceeds remaining budget %d", ErrBundleTooLarge, hdr.Name, hdr.Size, remaining)
			}
			totalBytes += hdr.Size
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o750); err != nil {
				return fmt.Errorf("create dir %s: %w", hdr.Name, err)
			}
		case tar.TypeReg:
			copyLimit := hdr.Size
			if copyLimit < 0 {
				copyLimit = 0
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return fmt.Errorf("create parent %s: %w", hdr.Name, err)
			}
			dst := target
			if _, err := os.Stat(target); err == nil {
				// Dual-copy: preserve both versions on conflict (DRAFT-01).
				dst = target + ".devstrap-conflict"
			}
			//nolint:gosec // Path validated within cleanDest; size bounded by the aggregate budget charged above.
			out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return fmt.Errorf("create %s: %w", hdr.Name, err)
			}
			if _, err := io.Copy(out, io.LimitReader(tr, copyLimit)); err != nil {
				_ = out.Close()
				return fmt.Errorf("write %s: %w", hdr.Name, err)
			}
			if err := out.Close(); err != nil {
				return fmt.Errorf("close %s: %w", hdr.Name, err)
			}
		}
	}
	return nil
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// isSecretPath mirrors the scanner's secret-looking detector so a bundle never
// carries a plaintext secret or private key (DRAFT-02).
func isSecretPath(rel string) bool {
	base := filepath.Base(rel)
	switch base {
	case ".env", "credentials.json", "service-account.json", "id_rsa", "id_ed25519":
		return true
	}
	if strings.HasPrefix(base, ".env.") && base != ".env.example" && base != ".env.template" && base != ".env.schema" {
		return true
	}
	if strings.HasSuffix(base, ".pem") {
		return true
	}
	return strings.HasSuffix(rel, "/.snowflake/config.toml") || strings.HasSuffix(rel, "/.aws/credentials") || strings.Contains(base, "service-account")
}
