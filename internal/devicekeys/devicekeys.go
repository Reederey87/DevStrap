package devicekeys

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"filippo.io/age"

	"github.com/Reederey87/DevStrap/internal/platform"
)

// Custody names where a HybridStore keeps its secret material. It is a
// per-machine decision recorded once at init (P6-XP-04) and honored on every
// later run so a store never silently migrates custody backends:
//
//   - CustodyKeychain: the OS keychain is authoritative. If it is unreachable,
//     operations fail closed rather than falling back to the file store — that
//     silent downgrade is exactly the split-custody wedge P6-XP-04 fixes.
//   - CustodyFile: the 0600 file store is authoritative and the keychain is
//     never consulted (headless/CI hosts, or DEVSTRAP_NO_KEYCHAIN=1).
//   - CustodyUnset: no decision recorded yet (a pre-P6-XP-04 store). Legacy
//     hybrid behavior applies: prefer the keychain, fall back to the file store
//     when the backend is unavailable. The published-key mint guard still holds,
//     so even this path can never mint a divergent identity.
type Custody string

const (
	CustodyUnset    Custody = ""
	CustodyKeychain Custody = "keychain"
	CustodyFile     Custody = "file"
)

// ErrKeychainUnreachable wraps a keychain error that means the backend could
// not be reached (unsupported platform, or a missing Secret Service / D-Bus
// session), as opposed to a secret that is genuinely absent. Load paths return
// it so callers can distinguish "custody unreachable" from "key not found",
// and mint paths refuse to generate a divergent identity when they see it
// (P6-XP-04).
var ErrKeychainUnreachable = errors.New("keychain backend unreachable")

type Identity struct {
	Private   string
	Recipient string
}

type SigningIdentity struct {
	Private string
	Public  string
}

type FileStore struct {
	Dir string
}

type SecretBackend interface {
	Store(ctx context.Context, service, account string, secret []byte) error
	Load(ctx context.Context, service, account string) ([]byte, error)
	Delete(ctx context.Context, service, account string) error
}

type HybridStore struct {
	File    FileStore
	Secret  SecretBackend
	Custody Custody
}

const keychainService = "devstrap.device-identity"

// probeAccount is a never-stored account name used by Probe to test keychain
// reachability with a read-only lookup.
const probeAccount = "__devstrap_custody_probe__"

func NewFileStore(dir string) FileStore {
	return FileStore{Dir: dir}
}

func NewHybridStore(dir string, secret SecretBackend) HybridStore {
	return HybridStore{File: NewFileStore(dir), Secret: secret}
}

// WithCustody returns a copy of the store pinned to the recorded custody
// backend (P6-XP-04). Callers resolve the recorded decision (and any
// DEVSTRAP_NO_KEYCHAIN override) and stamp it here before minting or reading.
func (s HybridStore) WithCustody(c Custody) HybridStore {
	s.Custody = c
	return s
}

// useKeychain reports whether the keychain backend should be consulted at all:
// only when a backend is wired and custody is not pinned to files.
func (s HybridStore) useKeychain() bool {
	return s.Secret != nil && s.Custody != CustodyFile
}

// requireKeychain reports whether custody forbids any file fallback: a
// keychain-custody store must fail closed rather than silently degrade.
func (s HybridStore) requireKeychain() bool {
	return s.Custody == CustodyKeychain
}

// Probe classifies the current keychain reachability into a custody decision
// with a single read-only lookup, without side effects. It is used once at init
// to record the custody backend (P6-XP-04). A nil backend or an unreachable
// Secret Service resolves to file custody; a reachable backend (the probe
// account is expectedly absent) resolves to keychain custody.
func (s HybridStore) Probe(ctx context.Context) Custody {
	if s.Secret == nil {
		return CustodyFile
	}
	_, err := s.Secret.Load(ctx, keychainService, probeAccount)
	switch {
	case err == nil:
		// Unexpected hit, but the backend is clearly reachable.
		return CustodyKeychain
	case keychainUnreachable(err):
		return CustodyFile
	case keychainSecretMissing(err):
		return CustodyKeychain
	default:
		// A live backend that returned some other error (locked, busy). Treat it
		// as reachable so custody stays keychain and later failures fail closed
		// rather than silently using files.
		return CustodyKeychain
	}
}

func NewIdentity() (Identity, error) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		return Identity{}, err
	}
	return Identity{Private: identity.String(), Recipient: identity.Recipient().String()}, nil
}

func NewSigningIdentity() (SigningIdentity, error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return SigningIdentity{}, err
	}
	return SigningIdentity{
		Private: "ed25519:" + base64.StdEncoding.EncodeToString(privateKey),
		Public:  "ed25519:" + base64.StdEncoding.EncodeToString(publicKey),
	}, nil
}

func (s FileStore) Ensure(deviceID string) (Identity, bool, error) {
	if err := validateDeviceID(deviceID); err != nil {
		return Identity{}, false, err
	}
	existing, err := s.Read(deviceID)
	if err == nil {
		return existing, false, nil
	}
	if !os.IsNotExist(err) {
		return Identity{}, false, err
	}
	identity, err := NewIdentity()
	if err != nil {
		return Identity{}, false, err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return Identity{}, false, fmt.Errorf("create device key directory: %w", err)
	}
	path := s.path(deviceID)
	if err := os.WriteFile(path, []byte(identity.Private+"\n"), 0o600); err != nil {
		return Identity{}, false, fmt.Errorf("write device identity: %w", err)
	}
	return identity, true, nil
}

// Ensure ensures a device age identity exists, minting one only when none is
// stored. publishedRecipient is the device's already-published age recipient
// ("" if none); when it is set and the keychain is unreachable, Ensure refuses
// to mint a divergent identity rather than wedge custody (P6-XP-04).
func (s HybridStore) Ensure(ctx context.Context, deviceID, publishedRecipient string) (Identity, bool, error) {
	existing, err := s.Read(ctx, deviceID)
	if err == nil {
		return existing, false, nil
	}
	if guard := s.mintGuard("age", publishedRecipient, err); guard != nil {
		return Identity{}, false, guard
	}
	identity, err := NewIdentity()
	if err != nil {
		return Identity{}, false, err
	}
	if _, err := s.storeSecretCustody(ctx, ageAccount(deviceID), identity.Private, "device age key", "device_id", deviceID, func() error {
		return s.File.writeIdentity(deviceID, identity)
	}); err != nil {
		return Identity{}, false, err
	}
	return identity, true, nil
}

func (s FileStore) Read(deviceID string) (Identity, error) {
	if err := validateDeviceID(deviceID); err != nil {
		return Identity{}, err
	}
	raw, err := os.ReadFile(s.path(deviceID))
	if err != nil {
		return Identity{}, err
	}
	private := strings.TrimSpace(string(raw))
	identity, err := age.ParseX25519Identity(private)
	if err != nil {
		return Identity{}, fmt.Errorf("parse device identity: %w", err)
	}
	return Identity{Private: private, Recipient: identity.Recipient().String()}, nil
}

func (s HybridStore) Read(ctx context.Context, deviceID string) (Identity, error) {
	if err := validateDeviceID(deviceID); err != nil {
		return Identity{}, err
	}
	raw, err := s.resolveSecret(ctx, ageAccount(deviceID), s.File.path(deviceID), func() (string, error) {
		id, ferr := s.File.Read(deviceID)
		return id.Private, ferr
	})
	if err != nil {
		return Identity{}, err
	}
	return parseAgeIdentity(raw)
}

func (s FileStore) EnsureSigning(deviceID string) (SigningIdentity, bool, error) {
	if err := validateDeviceID(deviceID); err != nil {
		return SigningIdentity{}, false, err
	}
	existing, err := s.ReadSigning(deviceID)
	if err == nil {
		return existing, false, nil
	}
	if !os.IsNotExist(err) {
		return SigningIdentity{}, false, err
	}
	identity, err := NewSigningIdentity()
	if err != nil {
		return SigningIdentity{}, false, err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return SigningIdentity{}, false, fmt.Errorf("create device key directory: %w", err)
	}
	if err := os.WriteFile(s.signingPath(deviceID), []byte(identity.Private+"\n"), 0o600); err != nil {
		return SigningIdentity{}, false, fmt.Errorf("write device signing identity: %w", err)
	}
	return identity, true, nil
}

// EnsureSigning ensures a device signing identity exists, minting one only when
// none is stored. publishedPub is the device's already-published signing public
// key ("" if none). When the keychain is unreachable, or when a key is already
// published but its private half is absent, EnsureSigning refuses to mint a
// divergent identity — the split-custody wedge P6-XP-04 fixes.
func (s HybridStore) EnsureSigning(ctx context.Context, deviceID, publishedPub string) (SigningIdentity, bool, error) {
	existing, err := s.ReadSigning(ctx, deviceID)
	if err == nil {
		return existing, false, nil
	}
	if guard := s.mintGuard("signing", publishedPub, err); guard != nil {
		return SigningIdentity{}, false, guard
	}
	identity, err := NewSigningIdentity()
	if err != nil {
		return SigningIdentity{}, false, err
	}
	if _, err := s.storeSecretCustody(ctx, signingAccount(deviceID), identity.Private, "device signing key", "device_id", deviceID, func() error {
		return s.File.writeSigningIdentity(deviceID, identity)
	}); err != nil {
		return SigningIdentity{}, false, err
	}
	return identity, true, nil
}

func (s FileStore) ReadSigning(deviceID string) (SigningIdentity, error) {
	if err := validateDeviceID(deviceID); err != nil {
		return SigningIdentity{}, err
	}
	raw, err := os.ReadFile(s.signingPath(deviceID))
	if err != nil {
		return SigningIdentity{}, err
	}
	private := strings.TrimSpace(string(raw))
	privateKey, err := parsePrivateSigningKey(private)
	if err != nil {
		return SigningIdentity{}, err
	}
	publicKey, ok := privateKey.Public().(ed25519.PublicKey)
	if !ok {
		return SigningIdentity{}, fmt.Errorf("derive signing public key")
	}
	return SigningIdentity{
		Private: private,
		Public:  "ed25519:" + base64.StdEncoding.EncodeToString(publicKey),
	}, nil
}

func (s HybridStore) ReadSigning(ctx context.Context, deviceID string) (SigningIdentity, error) {
	if err := validateDeviceID(deviceID); err != nil {
		return SigningIdentity{}, err
	}
	raw, err := s.resolveSecret(ctx, signingAccount(deviceID), s.File.signingPath(deviceID), func() (string, error) {
		id, ferr := s.File.ReadSigning(deviceID)
		return id.Private, ferr
	})
	if err != nil {
		return SigningIdentity{}, err
	}
	return parseSigningIdentity(raw)
}

// wckAccount is the keychain account name for a Workspace Content Key
// (P4-SEC-07 / P6-SEC-02): wck.<workspace_id>.<epoch> for the legacy
// bare-epoch form (kid == ""), or wck.<workspace_id>.<epoch>.<kid> once the
// key is identified by its fingerprint. The secret 32-byte WCK never enters
// SQLite; it lives in the OS keychain when available and a 0600 file
// otherwise.
func wckAccount(workspaceID string, epoch int64, kid string) string {
	if kid == "" {
		return fmt.Sprintf("wck.%s.%d", workspaceID, epoch)
	}
	return fmt.Sprintf("wck.%s.%d.%s", workspaceID, epoch, kid)
}

func (s FileStore) wckPath(workspaceID string, epoch int64, kid string) string {
	//nolint:gosec // workspaceID is an internally-generated ws_<uuidv7> and is
	// validated against path separators; epoch is an int; kid is validated as
	// 64 lowercase hex chars or empty. No user-controlled path component
	// reaches this filename.
	if kid == "" {
		return filepath.Join(s.Dir, fmt.Sprintf("wck-%s-%d.key", workspaceID, epoch))
	}
	return filepath.Join(s.Dir, fmt.Sprintf("wck-%s-%d-%s.key", workspaceID, epoch, kid))
}

// WriteWCK persists a WCK to the file fallback store as base64 with mode 0600.
func (s FileStore) WriteWCK(workspaceID string, epoch int64, kid string, wck []byte) error {
	if err := validateWorkspaceID(workspaceID); err != nil {
		return err
	}
	if err := validateKID(kid); err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("create device key directory: %w", err)
	}
	if err := os.WriteFile(s.wckPath(workspaceID, epoch, kid), []byte(base64.StdEncoding.EncodeToString(wck)+"\n"), 0o600); err != nil {
		return fmt.Errorf("write workspace key: %w", err)
	}
	return nil
}

// ReadWCK loads a WCK from the file fallback store.
func (s FileStore) ReadWCK(workspaceID string, epoch int64, kid string) ([]byte, error) {
	if err := validateWorkspaceID(workspaceID); err != nil {
		return nil, err
	}
	if err := validateKID(kid); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(s.wckPath(workspaceID, epoch, kid))
	if err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
}

// StoreWCK persists a 32-byte Workspace Content Key for the given workspace,
// epoch, and kid (P4-SEC-07 / P6-SEC-02), reusing the keychain/file custody
// path used for device identities. kid is the non-secret fingerprint
// hex(sha256(wck)); pass "" only for the legacy bare-epoch form. The
// keychain is preferred; a 0600 file fallback is used only when the keychain
// is genuinely unavailable (DEVSTRAP_NO_KEYCHAIN or a missing Secret
// Service), so headless/CI runs remain usable. A present-but-failing
// keychain fails closed (SECR-04/SECU-01).
func (s HybridStore) StoreWCK(ctx context.Context, workspaceID string, epoch int64, kid string, wck []byte) error {
	if err := validateWorkspaceID(workspaceID); err != nil {
		return err
	}
	if err := validateKID(kid); err != nil {
		return err
	}
	enc := base64.StdEncoding.EncodeToString(wck)
	_, err := s.storeSecretCustody(ctx, wckAccount(workspaceID, epoch, kid), enc, "workspace key", "workspace_id", workspaceID, func() error {
		return s.File.WriteWCK(workspaceID, epoch, kid, wck)
	})
	return err
}

// LoadWCK loads the WCK for a workspace+epoch+kid, or an error wrapping
// os.ErrNotExist if this device does not hold it. The decorator calls this
// during Pull to obtain the key for an enc.v1 event's epoch.
func (s HybridStore) LoadWCK(ctx context.Context, workspaceID string, epoch int64, kid string) ([]byte, error) {
	if err := validateWorkspaceID(workspaceID); err != nil {
		return nil, err
	}
	if err := validateKID(kid); err != nil {
		return nil, err
	}
	raw, err := s.resolveSecret(ctx, wckAccount(workspaceID, epoch, kid), s.File.wckPath(workspaceID, epoch, kid), func() (string, error) {
		b, ferr := s.File.ReadWCK(workspaceID, epoch, kid)
		if ferr != nil {
			return "", ferr
		}
		return base64.StdEncoding.EncodeToString(b), nil
	})
	if err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(raw)
}

// HubS3Credentials is the hub S3/R2 credential pair kept in local custody
// (P6-HUB-02): one JSON blob per workspace under the device-identity keychain
// service, with a 0600 file fallback. The secret access key never enters
// SQLite, config.yaml, or logs — plaintext env remains only a CI/override
// fallback resolved by the CLI layer.
type HubS3Credentials struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
}

// hubS3Account is the keychain account name for a workspace's hub S3
// credential blob (P6-HUB-02).
func hubS3Account(workspaceID string) string {
	return "hub-s3." + workspaceID
}

func (s FileStore) hubS3Path(workspaceID string) string {
	//nolint:gosec // workspaceID is an internally-generated ws_<uuidv7> and is
	// validated against path separators; no user-controlled path component
	// reaches this filename.
	return filepath.Join(s.Dir, "hub-s3-"+workspaceID+".json")
}

// WriteHubS3Credentials persists the credential blob to the file fallback
// store with mode 0600.
func (s FileStore) WriteHubS3Credentials(workspaceID string, creds HubS3Credentials) error {
	if err := validateWorkspaceID(workspaceID); err != nil {
		return err
	}
	raw, err := json.Marshal(creds)
	if err != nil {
		return fmt.Errorf("marshal hub s3 credentials: %w", err)
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("create device key directory: %w", err)
	}
	if err := os.WriteFile(s.hubS3Path(workspaceID), raw, 0o600); err != nil {
		return fmt.Errorf("write hub s3 credentials: %w", err)
	}
	return nil
}

// ReadHubS3Credentials loads the credential blob from the file fallback store.
func (s FileStore) ReadHubS3Credentials(workspaceID string) (HubS3Credentials, error) {
	if err := validateWorkspaceID(workspaceID); err != nil {
		return HubS3Credentials{}, err
	}
	raw, err := os.ReadFile(s.hubS3Path(workspaceID))
	if err != nil {
		return HubS3Credentials{}, err
	}
	var creds HubS3Credentials
	if err := json.Unmarshal(raw, &creds); err != nil {
		return HubS3Credentials{}, fmt.Errorf("parse hub s3 credentials: %w", err)
	}
	return creds, nil
}

// DeleteHubS3Credentials removes the credential blob from the file fallback
// store; a missing file is not an error.
func (s FileStore) DeleteHubS3Credentials(workspaceID string) error {
	if err := validateWorkspaceID(workspaceID); err != nil {
		return err
	}
	if err := os.Remove(s.hubS3Path(workspaceID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete hub s3 credentials: %w", err)
	}
	return nil
}

// StoreHubS3Credentials persists the hub S3 credential pair (P6-HUB-02),
// reusing the keychain/file custody path used for device identities and WCKs:
// keychain preferred, 0600 file fallback only when the keychain is genuinely
// unavailable (DEVSTRAP_NO_KEYCHAIN or a missing Secret Service); a
// present-but-failing keychain fails closed (SECR-04/SECU-01). Returns where
// the credentials landed ("keychain" or "file") so the CLI can tell the user.
func (s HybridStore) StoreHubS3Credentials(ctx context.Context, workspaceID string, creds HubS3Credentials) (string, error) {
	if err := validateWorkspaceID(workspaceID); err != nil {
		return "", err
	}
	raw, err := json.Marshal(creds)
	if err != nil {
		return "", fmt.Errorf("marshal hub s3 credentials: %w", err)
	}
	return s.storeSecretCustody(ctx, hubS3Account(workspaceID), string(raw), "hub s3 credentials", "workspace_id", workspaceID, func() error {
		return s.File.WriteHubS3Credentials(workspaceID, creds)
	})
}

// LoadHubS3Credentials loads the stored hub S3 credential pair, or an error
// wrapping os.ErrNotExist when none is stored.
func (s HybridStore) LoadHubS3Credentials(ctx context.Context, workspaceID string) (HubS3Credentials, error) {
	if err := validateWorkspaceID(workspaceID); err != nil {
		return HubS3Credentials{}, err
	}
	raw, err := s.resolveSecret(ctx, hubS3Account(workspaceID), s.File.hubS3Path(workspaceID), func() (string, error) {
		creds, ferr := s.File.ReadHubS3Credentials(workspaceID)
		if ferr != nil {
			return "", ferr
		}
		b, merr := json.Marshal(creds)
		return string(b), merr
	})
	if err != nil {
		return HubS3Credentials{}, err
	}
	var creds HubS3Credentials
	if err := json.Unmarshal([]byte(raw), &creds); err != nil {
		return HubS3Credentials{}, fmt.Errorf("parse hub s3 credentials: %w", err)
	}
	return creds, nil
}

// DeleteHubS3Credentials removes the stored pair from both custody backends
// (hub logout). Missing entries are not errors.
func (s HybridStore) DeleteHubS3Credentials(ctx context.Context, workspaceID string) error {
	if err := validateWorkspaceID(workspaceID); err != nil {
		return err
	}
	if s.useKeychain() {
		// A missing entry or an unreachable/unsupported backend is tolerated
		// here; only a live-backend hard failure surfaces.
		if err := s.Secret.Delete(ctx, keychainService, hubS3Account(workspaceID)); err != nil &&
			!errors.Is(err, os.ErrNotExist) && !IsKeychainUnavailable(err) {
			return fmt.Errorf("delete hub s3 credentials from keychain: %w", err)
		}
	}
	return s.File.DeleteHubS3Credentials(workspaceID)
}

// BackupEpoch names one held Workspace Content Key by its epoch and kid so a
// full backup can escrow it (P6-DATA-04). It mirrors the (epoch, kid) tuple the
// store records in workspace_keys.
type BackupEpoch struct {
	Epoch int64
	KID   string
}

// ExportForBackup reads every secret this device holds for the given device and
// workspace — the device age identity, the device signing identity, each held
// WCK epoch, and the hub S3 credentials when present — from the active custody
// backend and writes them into dstDir using the FileStore on-disk format with
// mode 0600 (P6-DATA-04). Because it reads through the recorded custody backend
// (via HybridStore.Read/ReadSigning/LoadWCK/LoadHubS3Credentials), it captures
// keychain-held material as portable escrow files for a full backup, not just
// the file-custody KeyDir. A basename already present in dstDir is left
// untouched, so it is safe to run over a directory the caller has pre-populated.
//
// It returns the basenames written (sorted). A REQUIRED secret — the device age
// or signing identity, or any held WCK epoch — that cannot be read is a hard
// error naming the secret: a "full" backup must never silently omit key
// material. Hub S3 credentials are optional (a workspace may have no hub
// configured): a genuinely-absent entry is skipped, but any other read failure
// is fatal. No secret bytes are logged.
func (s HybridStore) ExportForBackup(ctx context.Context, dstDir, deviceID, workspaceID string, epochs []BackupEpoch) ([]string, error) {
	if err := validateDeviceID(deviceID); err != nil {
		return nil, err
	}
	if err := validateWorkspaceID(workspaceID); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dstDir, 0o700); err != nil {
		return nil, fmt.Errorf("create key escrow directory: %w", err)
	}
	dst := FileStore{Dir: dstDir}
	var written []string
	writeOnce := func(basename string, write func() error) error {
		if _, err := os.Stat(filepath.Join(dstDir, basename)); err == nil {
			return nil // already staged (e.g. a KeyDir copy); do not overwrite
		}
		if err := write(); err != nil {
			return err
		}
		written = append(written, basename)
		return nil
	}

	identity, err := s.Read(ctx, deviceID)
	if err != nil {
		return nil, fmt.Errorf("escrow device age identity: %w", err)
	}
	if err := writeOnce(filepath.Base(dst.path(deviceID)), func() error {
		return dst.writeIdentity(deviceID, identity)
	}); err != nil {
		return nil, err
	}

	signing, err := s.ReadSigning(ctx, deviceID)
	if err != nil {
		return nil, fmt.Errorf("escrow device signing identity: %w", err)
	}
	if err := writeOnce(filepath.Base(dst.signingPath(deviceID)), func() error {
		return dst.writeSigningIdentity(deviceID, signing)
	}); err != nil {
		return nil, err
	}

	for _, e := range epochs {
		wck, err := s.LoadWCK(ctx, workspaceID, e.Epoch, e.KID)
		if err != nil {
			return nil, fmt.Errorf("escrow workspace key epoch %d: %w", e.Epoch, err)
		}
		if err := writeOnce(filepath.Base(dst.wckPath(workspaceID, e.Epoch, e.KID)), func() error {
			return dst.WriteWCK(workspaceID, e.Epoch, e.KID, wck)
		}); err != nil {
			return nil, err
		}
	}

	creds, err := s.LoadHubS3Credentials(ctx, workspaceID)
	switch {
	case err == nil:
		if err := writeOnce(filepath.Base(dst.hubS3Path(workspaceID)), func() error {
			return dst.WriteHubS3Credentials(workspaceID, creds)
		}); err != nil {
			return nil, err
		}
	case errors.Is(err, os.ErrNotExist):
		// No hub credentials in local custody; nothing to escrow.
	default:
		return nil, fmt.Errorf("escrow hub s3 credentials: %w", err)
	}

	sort.Strings(written)
	return written, nil
}

func Sign(privateKey, domain string, message []byte) (string, error) {
	key, err := parsePrivateSigningKey(privateKey)
	if err != nil {
		return "", err
	}
	signature := ed25519.Sign(key, domainSeparatedMessage(domain, message))
	return "ed25519:" + base64.StdEncoding.EncodeToString(signature), nil
}

func Verify(publicKey, signature, domain string, message []byte) error {
	pub, err := parsePublicSigningKey(publicKey)
	if err != nil {
		return err
	}
	sig, err := parsePrefixedBytes(signature, ed25519.SignatureSize, "signature")
	if err != nil {
		return err
	}
	if !ed25519.Verify(pub, domainSeparatedMessage(domain, message), sig) {
		return fmt.Errorf("ed25519 signature verification failed")
	}
	return nil
}

func (s FileStore) path(deviceID string) string {
	return filepath.Join(s.Dir, deviceID+".agekey")
}

func (s FileStore) writeIdentity(deviceID string, identity Identity) error {
	if err := validateDeviceID(deviceID); err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("create device key directory: %w", err)
	}
	if err := os.WriteFile(s.path(deviceID), []byte(identity.Private+"\n"), 0o600); err != nil {
		return fmt.Errorf("write device identity: %w", err)
	}
	return nil
}

func (s FileStore) writeSigningIdentity(deviceID string, identity SigningIdentity) error {
	if err := validateDeviceID(deviceID); err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("create device key directory: %w", err)
	}
	if err := os.WriteFile(s.signingPath(deviceID), []byte(identity.Private+"\n"), 0o600); err != nil {
		return fmt.Errorf("write device signing identity: %w", err)
	}
	return nil
}

func (s HybridStore) storeSecret(ctx context.Context, account, secret string) error {
	if s.Secret == nil {
		return os.ErrNotExist
	}
	if err := s.Secret.Store(ctx, keychainService, account, []byte(secret)); err != nil {
		return err
	}
	return nil
}

// loadSecret loads a raw secret from the keychain backend, translating its
// error into the typed vocabulary the custody logic depends on (P6-XP-04):
//   - a genuinely-absent secret → os.ErrNotExist;
//   - an unreachable/unsupported backend → ErrKeychainUnreachable;
//   - anything else (a live-backend hard failure) → the error verbatim.
func (s HybridStore) loadSecret(ctx context.Context, account string) (string, error) {
	if s.Secret == nil {
		return "", os.ErrNotExist
	}
	raw, err := s.Secret.Load(ctx, keychainService, account)
	if err != nil {
		switch {
		case keychainSecretMissing(err):
			return "", os.ErrNotExist
		case keychainUnreachable(err):
			return "", fmt.Errorf("%w: %w", ErrKeychainUnreachable, err)
		default:
			return "", err
		}
	}
	return strings.TrimSpace(string(raw)), nil
}

// resolveSecret loads a secret across the custody backends and returns the raw
// stored string. It honors the recorded custody decision (P6-XP-04):
//   - under keychain custody the keychain is authoritative and no file is ever
//     read — a keychain failure (unreachable or hard) surfaces rather than
//     silently degrading;
//   - under file custody the keychain is skipped entirely;
//   - under unset (legacy) custody the keychain is preferred, and the file
//     store is used only as a fallback when a file is actually present — so an
//     unreachable keychain with no local file surfaces ErrKeychainUnreachable
//     ("custody unreachable") instead of a misleading "not found".
func (s HybridStore) resolveSecret(ctx context.Context, account, filePath string, readFile func() (string, error)) (string, error) {
	if s.useKeychain() {
		raw, err := s.loadSecret(ctx, account)
		if err == nil {
			return raw, nil
		}
		if s.requireKeychain() {
			return "", err
		}
		if fileMissing(filePath) {
			return "", err
		}
		// A local file exists: legacy/unset custody prefers it over surfacing
		// the keychain error (the documented pre-P6-XP-04 read residual).
	}
	return readFile()
}

// storeSecretCustody stores a secret honoring the recorded custody decision and
// reports where it landed ("keychain" or "file") (P6-XP-04):
//   - keychain custody must land in the keychain; any failure fails closed;
//   - file custody writes the file store and never touches the keychain;
//   - unset (legacy) custody prefers the keychain and falls back to the file
//     store only when the backend is unavailable — a live-backend hard failure
//     still fails closed (SECR-04/SECU-01).
func (s HybridStore) storeSecretCustody(ctx context.Context, account, secret, label, logKey, logVal string, writeFile func() error) (string, error) {
	if s.useKeychain() {
		err := s.storeSecret(ctx, account, secret)
		switch {
		case err == nil:
			return "keychain", nil
		case s.requireKeychain():
			return "", fmt.Errorf("store %s in keychain (custody=keychain, refusing file fallback): %w", label, err)
		case keychainUnreachable(err) || keychainSecretMissing(err):
			slog.Warn("keychain unavailable; writing "+label+" to file fallback", logKey, logVal)
		default:
			// Live-backend hard failure: fail closed rather than downgrade.
			return "", fmt.Errorf("store %s in keychain: %w", label, err)
		}
	}
	if err := writeFile(); err != nil {
		return "", err
	}
	return "file", nil
}

// IsKeychainUnavailable reports whether a keychain error means the backend is
// missing/not-present (so an operation may tolerate it and use the file store)
// rather than a live-backend hard failure. It is the coarse predicate used by
// tolerant paths (e.g. delete); mint/store paths use the finer
// keychainUnreachable / keychainSecretMissing split so a divergent identity is
// never minted (P6-XP-04).
func IsKeychainUnavailable(err error) bool {
	return keychainSecretMissing(err) || keychainUnreachable(err)
}

// keychainSecretMissing reports that the keychain is reachable but holds no such
// secret.
func keychainSecretMissing(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, platform.ErrSecretNotFound)
}

// keychainUnreachable reports that the keychain backend could not be reached
// (unsupported platform, or a missing Secret Service / D-Bus session). The
// platform adapter types these as ErrUnsupported (see mapKeyringError), and the
// store's own ErrKeychainUnreachable wraps ErrUnsupported, so both match.
func keychainUnreachable(err error) bool {
	return errors.Is(err, platform.ErrUnsupported) || errors.Is(err, ErrKeychainUnreachable)
}

// mintGuard decides whether a mint may proceed given the error from reading the
// existing key (P6-XP-04). It returns a refusal error when minting would risk a
// divergent or silently-downgraded identity, or nil when minting into the
// recorded custody backend is safe:
//   - keychain unreachable + a key already published, or keychain custody: refuse
//     (the split-custody wedge, and the no-silent-downgrade rule);
//   - keychain unreachable + nothing published under legacy/unset custody: allow
//     the file fallback (today's headless behavior, preserved);
//   - reachable but genuinely absent + a key already published: refuse (custody
//     lost the private half; a replacement would diverge);
//   - reachable but genuinely absent + nothing published: allow the mint;
//   - any other (live-backend hard) error: propagate it.
func (s HybridStore) mintGuard(kind, published string, readErr error) error {
	switch {
	case errors.Is(readErr, ErrKeychainUnreachable):
		if published != "" || s.requireKeychain() {
			return mintRefusedUnreachable(kind, published, readErr)
		}
		return nil
	case errors.Is(readErr, os.ErrNotExist):
		if published != "" {
			return mintRefusedPublishedAbsent(kind, published)
		}
		return nil
	default:
		return readErr
	}
}

// mintRefusedUnreachable is returned when the keychain is unreachable and
// minting would risk a divergent identity (P6-XP-04). The remedy names both
// escape hatches: run from a desktop session, or opt into file custody.
func mintRefusedUnreachable(kind, published string, err error) error {
	if published != "" {
		return fmt.Errorf(
			"device %s key exists (%s) but the keychain is unreachable (session bus missing?); "+
				"refusing to mint a divergent key — run from your desktop session, or set %s=1 and migrate the key file: %w",
			kind, published, platform.NoKeychainEnv, err)
	}
	return fmt.Errorf(
		"keychain unreachable; refusing to mint a divergent device %s key — "+
			"run from your desktop session, or set %s=1 to use file custody: %w",
		kind, platform.NoKeychainEnv, err)
}

// mintRefusedPublishedAbsent is returned when a key is already published but its
// private half is missing from a reachable custody backend (P6-XP-04): minting a
// replacement would diverge from the published public key.
func mintRefusedPublishedAbsent(kind, published string) error {
	return fmt.Errorf(
		"device %s key %s is already published but its private half is missing from custody; "+
			"refusing to mint a divergent key — restore the key file/keychain entry, or set %s=1 and migrate it",
		kind, published, platform.NoKeychainEnv)
}

func ageAccount(deviceID string) string {
	return deviceID + ".age"
}

func signingAccount(deviceID string) string {
	return deviceID + ".signing"
}

func fileMissing(path string) bool {
	_, err := os.Stat(path)
	return os.IsNotExist(err)
}

func (s FileStore) signingPath(deviceID string) string {
	return filepath.Join(s.Dir, deviceID+".signing.key")
}

func validateDeviceID(deviceID string) error {
	if deviceID == "" || strings.ContainsAny(deviceID, `/\`) {
		return fmt.Errorf("invalid device id %q", deviceID)
	}
	return nil
}

// validateWorkspaceID guards WCK file/keychain account names against path
// traversal. workspaceID is an internally-generated ws_<uuidv7>, but defending
// here keeps the file fallback safe even if a future caller passes an
// unvalidated value.
func validateWorkspaceID(workspaceID string) error {
	if workspaceID == "" || strings.ContainsAny(workspaceID, `/\`) {
		return fmt.Errorf("invalid workspace id %q", workspaceID)
	}
	return nil
}

// validateKID guards WCK file/keychain account names against path traversal
// and injection (P6-SEC-02). kid is either empty (the legacy bare-epoch
// form) or exactly 64 lowercase hex characters
// (hex(sha256(wck))) — the non-secret fingerprint of a Workspace Content
// Key. Every entry point that turns (workspace_id, epoch, kid) into an
// account name or file path must call this first.
func validateKID(kid string) error {
	if kid == "" {
		return nil
	}
	if len(kid) != 64 {
		return fmt.Errorf("invalid workspace key id %q: must be 64 lowercase hex characters", kid)
	}
	for _, r := range kid {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return fmt.Errorf("invalid workspace key id %q: must be 64 lowercase hex characters", kid)
		}
	}
	return nil
}

func parsePrivateSigningKey(value string) (ed25519.PrivateKey, error) {
	raw, err := parsePrefixedBytes(value, ed25519.PrivateKeySize, "private signing key")
	if err != nil {
		return nil, err
	}
	return ed25519.PrivateKey(raw), nil
}

func parseAgeIdentity(private string) (Identity, error) {
	identity, err := age.ParseX25519Identity(strings.TrimSpace(private))
	if err != nil {
		return Identity{}, fmt.Errorf("parse device identity: %w", err)
	}
	return Identity{Private: strings.TrimSpace(private), Recipient: identity.Recipient().String()}, nil
}

func parseSigningIdentity(private string) (SigningIdentity, error) {
	private = strings.TrimSpace(private)
	privateKey, err := parsePrivateSigningKey(private)
	if err != nil {
		return SigningIdentity{}, err
	}
	publicKey, ok := privateKey.Public().(ed25519.PublicKey)
	if !ok {
		return SigningIdentity{}, fmt.Errorf("derive signing public key")
	}
	return SigningIdentity{
		Private: private,
		Public:  "ed25519:" + base64.StdEncoding.EncodeToString(publicKey),
	}, nil
}

func parsePublicSigningKey(value string) (ed25519.PublicKey, error) {
	raw, err := parsePrefixedBytes(value, ed25519.PublicKeySize, "public signing key")
	if err != nil {
		return nil, err
	}
	return ed25519.PublicKey(raw), nil
}

func parsePrefixedBytes(value string, wantLen int, name string) ([]byte, error) {
	raw, ok := strings.CutPrefix(strings.TrimSpace(value), "ed25519:")
	if !ok {
		return nil, fmt.Errorf("%s must use ed25519: prefix", name)
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", name, err)
	}
	if len(decoded) != wantLen {
		return nil, fmt.Errorf("%s length = %d, want %d", name, len(decoded), wantLen)
	}
	return decoded, nil
}

func domainSeparatedMessage(domain string, message []byte) []byte {
	out := make([]byte, 0, len(domain)+1+len(message))
	out = append(out, domain...)
	out = append(out, 0)
	out = append(out, message...)
	return out
}
