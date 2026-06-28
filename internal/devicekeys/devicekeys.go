package devicekeys

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
)

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
	File   FileStore
	Secret SecretBackend
}

const keychainService = "devstrap.device-identity"

func NewFileStore(dir string) FileStore {
	return FileStore{Dir: dir}
}

func NewHybridStore(dir string, secret SecretBackend) HybridStore {
	return HybridStore{File: NewFileStore(dir), Secret: secret}
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

func (s HybridStore) Ensure(ctx context.Context, deviceID string) (Identity, bool, error) {
	existing, err := s.Read(ctx, deviceID)
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Identity{}, false, err
	}
	identity, err := NewIdentity()
	if err != nil {
		return Identity{}, false, err
	}
	// SECR-04/SECU-01: only fall back to the plaintext file store when the
	// keychain is genuinely unavailable; a present-but-failing keychain must
	// fail closed so a transient error never silently downgrades key custody.
	if err := s.storeSecret(ctx, ageAccount(deviceID), identity.Private); err == nil {
		return identity, true, nil
	} else if !IsKeychainUnavailable(err) {
		return Identity{}, false, fmt.Errorf("store device key in keychain: %w", err)
	}
	slog.Warn("keychain unavailable; writing device age key to file fallback", "device_id", deviceID)
	if err := s.File.writeIdentity(deviceID, identity); err != nil {
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
	if s.Secret != nil {
		private, err := s.loadSecret(ctx, ageAccount(deviceID))
		if err == nil {
			return parseAgeIdentity(private)
		}
		if !errors.Is(err, os.ErrNotExist) && fileMissing(s.File.path(deviceID)) {
			return Identity{}, err
		}
	}
	return s.File.Read(deviceID)
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

func (s HybridStore) EnsureSigning(ctx context.Context, deviceID string) (SigningIdentity, bool, error) {
	existing, err := s.ReadSigning(ctx, deviceID)
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return SigningIdentity{}, false, err
	}
	identity, err := NewSigningIdentity()
	if err != nil {
		return SigningIdentity{}, false, err
	}
	// SECR-04/SECU-01: only fall back to the plaintext file store when the
	// keychain is genuinely unavailable; fail closed on other errors.
	if err := s.storeSecret(ctx, signingAccount(deviceID), identity.Private); err == nil {
		return identity, true, nil
	} else if !IsKeychainUnavailable(err) {
		return SigningIdentity{}, false, fmt.Errorf("store device signing key in keychain: %w", err)
	}
	slog.Warn("keychain unavailable; writing device signing key to file fallback", "device_id", deviceID)
	if err := s.File.writeSigningIdentity(deviceID, identity); err != nil {
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
	if s.Secret != nil {
		private, err := s.loadSecret(ctx, signingAccount(deviceID))
		if err == nil {
			return parseSigningIdentity(private)
		}
		if !errors.Is(err, os.ErrNotExist) && fileMissing(s.File.signingPath(deviceID)) {
			return SigningIdentity{}, err
		}
	}
	return s.File.ReadSigning(deviceID)
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

func (s HybridStore) loadSecret(ctx context.Context, account string) (string, error) {
	if s.Secret == nil {
		return "", os.ErrNotExist
	}
	raw, err := s.Secret.Load(ctx, keychainService, account)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || keychainUnavailable(err) {
			return "", os.ErrNotExist
		}
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// IsKeychainUnavailable reports whether a keychain error means the backend is
// missing/not-present (so the file store should be used) rather than a genuine
// failure. Covers macOS "not found", an unsupported platform, and a Linux
// Secret Service / D-Bus that is not running (common on headless/CI hosts).
func IsKeychainUnavailable(err error) bool {
	return keychainUnavailable(err)
}

// keychainUnavailable reports whether a keychain error means the backend is
// missing/not-present (so the file store should be used) rather than a genuine
// failure. Covers macOS "not found", an unsupported platform, and a Linux
// Secret Service / D-Bus that is not running (common on headless/CI hosts).
func keychainUnavailable(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"not found",
		"unsupported",
		"was not provided", // dbus: org.freedesktop.secrets not provided
		"org.freedesktop.secrets",
		"no such interface",
		"connection refused",
		"dbus",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
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
