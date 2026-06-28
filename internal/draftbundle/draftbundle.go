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

// Limits are the enforced size and file-count bounds for a draft bundle
// (DRAFT-04).
type Limits struct {
	MaxBytes int64
	MaxFiles int64
}

// Snapshot records the metadata of a packed draft bundle.
type Snapshot struct {
	BlobRef    string // age_blob:<sha256>
	ByteSize   int64
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
		limits.MaxFiles = 5000
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
// gunzips and untars it into dest (DRAFT-02 hydrate). Files are written 0600;
// directories 0750. Existing files are not overwritten (dual-copy conflict
// safety, decision #7).
func Extract(ciphertext []byte, identity, dest string) error {
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
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o750); err != nil {
				return fmt.Errorf("create dir %s: %w", hdr.Name, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return fmt.Errorf("create parent %s: %w", hdr.Name, err)
			}
			if _, err := os.Stat(target); err == nil {
				// Dual-copy: preserve both versions on conflict (DRAFT-01).
				conflict := target + ".devstrap-conflict"
				//nolint:gosec // Path validated within cleanDest; size bounded by tar header.
				cf, err := os.OpenFile(conflict, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
				if err != nil {
					return fmt.Errorf("create conflict %s: %w", hdr.Name, err)
				}
				if _, err := io.Copy(cf, io.LimitReader(tr, hdr.Size)); err != nil {
					_ = cf.Close()
					return fmt.Errorf("write conflict %s: %w", hdr.Name, err)
				}
				if err := cf.Close(); err != nil {
					return fmt.Errorf("close conflict %s: %w", hdr.Name, err)
				}
				continue
			}
			//nolint:gosec // The path is validated within cleanDest and the size is bounded by the tar header.
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return fmt.Errorf("create %s: %w", hdr.Name, err)
			}
			if _, err := io.Copy(out, io.LimitReader(tr, hdr.Size)); err != nil {
				_ = out.Close()
				return fmt.Errorf("write %s: %w", hdr.Name, err)
			}
			if err := out.Close(); err != nil {
				return fmt.Errorf("close %s: %w", hdr.Name, err)
			}
		default:
			// Skip symlinks, devices, and other special types for safety.
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
