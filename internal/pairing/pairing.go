// Package pairing encodes the one-paste device pairing code (P4-SEC-04 part 2):
// a single copy-paste blob replacing the seven hand-pasted values of the manual
// ceremony.
//
// Wire versions
//
//   - v1 (`devstrap-pair1:`): the original blob. DELIBERATELY UNAUTHENTICATED —
//     it carries NO fingerprint, so integrity comes entirely from the
//     out-of-band fingerprint comparison at approval time (the fingerprint is
//     DERIVED from the carried keys, never carried in the blob). Old binaries
//     still emit this; the current binary still decodes it exactly.
//   - v2 (`devstrap-pair2:`, P7-PROD-01): adds two OPTIONAL fields — the device
//     fingerprint (derived once at Encode time from the same keys) and an
//     optional hub URI — so ONE paste carries everything a fresh joiner needs
//     (`devstrap join`). The embedded fingerprint is a CONVENIENCE and a
//     corruption check, NOT cryptographic authentication: the blob is still
//     unauthenticated, so an attacker who can rewrite it in transit regenerates
//     a self-consistent fingerprint for substituted keys just as the legitimate
//     sender did. The separate out-of-band read-aloud comparison (still printed
//     to stderr, still accepted via `join --fingerprint`) remains the only path
//     that defends against a compromised paste channel.
//
// The version is authoritative from the `devstrap-pair<N>:` prefix; the inner
// `v` field must agree with it (a mismatch is a malformed/tampered blob).
package pairing

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"filippo.io/age"
	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/id"
)

// prefixStem is the common stem of every versioned prefix (`devstrap-pair<N>:`).
const prefixStem = "devstrap-pair"

// Version is the wire version Encode emits, and the newest version this binary
// can decode. Prefix is the matching current-version prefix.
const (
	Version = 2
	Prefix  = "devstrap-pair2:"
)

const maxBlobLen = 8192

type wireCode struct {
	V    int    `json:"v"`
	WS   string `json:"ws"`   // workspace id ws_<32 lower hex>
	Dev  string `json:"dev"`  // device id dev_<32 lower hex>
	Name string `json:"name"` // device display name
	OS   string `json:"os"`
	Arch string `json:"arch"`
	Age  string `json:"age"`           // age X25519 recipient
	Sig  string `json:"sig"`           // ed25519:<base64> signing public key
	FP   string `json:"fp,omitempty"`  // v2: fingerprint derived from Sig+Age (convenience + corruption check)
	Hub  string `json:"hub,omitempty"` // v2: hub URI so `devstrap join` can auto-configure the hub
}

type Code struct {
	WorkspaceID      string
	DeviceID         string
	Name             string
	OS               string
	Arch             string
	AgeRecipient     string
	SigningPublicKey string

	// Version is the wire version this Code was decoded from (1 or 2). Encode
	// ignores it and always emits the current Version.
	Version int
	// Fingerprint is the device fingerprint carried by a v2 code (empty for v1
	// or a v2 code that omitted it). When present it is guaranteed consistent
	// with the carried keys — Decode rejects a mismatch as corruption. Encode
	// ignores any caller-supplied value and re-derives it from the keys.
	Fingerprint string
	// HubURI is the hub URI carried by a v2 code, or empty when the founder had
	// no hub configured (or for a v1 code). Consumers configure their hub from
	// it; when empty they must set a hub themselves.
	HubURI string
}

// HasFingerprint reports whether the code carries an embedded fingerprint (a v2
// code from a founder that included it). Callers branch on this to decide
// whether to auto-trust or fall back to the manual out-of-band flow.
func (c Code) HasFingerprint() bool { return strings.TrimSpace(c.Fingerprint) != "" }

// HasHubURI reports whether the code carries a hub URI to auto-configure.
func (c Code) HasHubURI() bool { return strings.TrimSpace(c.HubURI) != "" }

func Encode(c Code) (string, error) {
	validated, err := validate(c)
	if err != nil {
		return "", err
	}
	// Derive the fingerprint from the keys being encoded — never trust a
	// caller-supplied value, so the embedded fingerprint is always
	// self-consistent with the carried keys.
	fp, err := devicekeys.Fingerprint(validated.SigningPublicKey, validated.AgeRecipient)
	if err != nil {
		return "", fmt.Errorf("derive fingerprint for pairing code: %w", err)
	}
	raw, err := json.Marshal(wireCode{
		V:    Version,
		WS:   validated.WorkspaceID,
		Dev:  validated.DeviceID,
		Name: validated.Name,
		OS:   validated.OS,
		Arch: validated.Arch,
		Age:  validated.AgeRecipient,
		Sig:  validated.SigningPublicKey,
		FP:   fp,
		Hub:  validated.HubURI,
	})
	if err != nil {
		return "", err
	}
	return Prefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

// Decode parses a pairing blob of any version this binary understands (v1 or
// v2). For a v1 blob — or a v2 blob that omitted the fingerprint — the returned
// Code reports no fingerprint, and consumers MUST derive and confirm the
// fingerprint out-of-band before approving the carried device. A v2 blob's
// embedded fingerprint is a convenience and a corruption check, not
// authentication (see the package comment).
func Decode(blob string) (Code, error) {
	blob = strings.TrimSpace(blob)
	if len(blob) > maxBlobLen {
		return Code{}, fmt.Errorf("pairing code exceeds the 8KB limit")
	}
	ver, payload, err := splitVersionedPrefix(blob)
	if err != nil {
		return Code{}, err
	}
	if ver > Version {
		return Code{}, fmt.Errorf("pairing code was created by a newer devstrap (v%d); upgrade this binary", ver)
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return Code{}, fmt.Errorf("pairing code is not valid base64url: %w", err)
	}
	var wire wireCode
	if err := json.Unmarshal(raw, &wire); err != nil {
		return Code{}, fmt.Errorf("pairing code payload is not valid JSON: %w", err)
	}
	// The prefix is authoritative; the inner `v` must agree, or the blob is
	// malformed/tampered.
	if wire.V != ver {
		return Code{}, fmt.Errorf("pairing code version field (v%d) does not match its devstrap-pair%d: prefix", wire.V, ver)
	}
	code, err := validate(Code{
		WorkspaceID:      wire.WS,
		DeviceID:         wire.Dev,
		Name:             wire.Name,
		OS:               wire.OS,
		Arch:             wire.Arch,
		AgeRecipient:     wire.Age,
		SigningPublicKey: wire.Sig,
		HubURI:           wire.Hub,
	})
	if err != nil {
		return Code{}, err
	}
	code.Version = ver
	if ver >= 2 && strings.TrimSpace(wire.FP) != "" {
		// The keys are already validated, so this derivation cannot fail.
		derived, derr := devicekeys.Fingerprint(code.SigningPublicKey, code.AgeRecipient)
		if derr != nil {
			return Code{}, fmt.Errorf("pairing code carries an invalid signing public key: %w", derr)
		}
		// Corruption check only: a mismatch means the blob was mangled in
		// transit (a full attacker would regenerate a matching fingerprint).
		// Store the canonical derived form regardless of the carried grouping.
		if !devicekeys.FingerprintEqual(wire.FP, derived) {
			return Code{}, fmt.Errorf("pairing code's embedded fingerprint does not match its keys (corrupted in transit); re-copy the code")
		}
		code.Fingerprint = derived
	}
	return code, nil
}

// splitVersionedPrefix parses the `devstrap-pair<N>:` prefix, returning the
// version number and the base64url payload after the colon.
func splitVersionedPrefix(blob string) (int, string, error) {
	notACode := fmt.Errorf("not a devstrap pairing code (expected the %s prefix)", Prefix)
	if !strings.HasPrefix(blob, prefixStem) {
		return 0, "", notACode
	}
	rest := blob[len(prefixStem):]
	colon := strings.IndexByte(rest, ':')
	if colon <= 0 {
		return 0, "", notACode
	}
	ver, err := strconv.Atoi(rest[:colon])
	if err != nil || ver < 1 {
		return 0, "", notACode
	}
	return ver, rest[colon+1:], nil
}

func validate(c Code) (Code, error) {
	c.WorkspaceID = strings.TrimSpace(c.WorkspaceID)
	c.DeviceID = strings.TrimSpace(c.DeviceID)
	c.Name = strings.TrimSpace(c.Name)
	c.OS = strings.TrimSpace(c.OS)
	c.Arch = strings.TrimSpace(c.Arch)
	c.AgeRecipient = strings.TrimSpace(c.AgeRecipient)
	c.SigningPublicKey = strings.TrimSpace(c.SigningPublicKey)
	c.HubURI = strings.TrimSpace(c.HubURI)

	if !id.Valid("ws", c.WorkspaceID) {
		return Code{}, fmt.Errorf("pairing code carries an invalid workspace id")
	}
	if !id.Valid("dev", c.DeviceID) {
		return Code{}, fmt.Errorf("pairing code carries an invalid device id")
	}
	if c.Name == "" {
		return Code{}, fmt.Errorf("pairing code carries an empty device name")
	}
	if c.OS == "" {
		return Code{}, fmt.Errorf("pairing code carries an empty device os")
	}
	if c.Arch == "" {
		return Code{}, fmt.Errorf("pairing code carries an empty device arch")
	}
	// The blob rides an untrusted channel and name/os/arch/hub later reach
	// terminal output (`devices list`, join hints), so reject control characters
	// here — an embedded newline or escape sequence must never survive decode
	// (post-#57 opus review, M2; hub added for P7-PROD-01).
	fields := map[string]string{"name": c.Name, "os": c.OS, "arch": c.Arch}
	if c.HubURI != "" {
		fields["hub uri"] = c.HubURI
	}
	for field, value := range fields {
		for _, r := range value {
			if r < 0x20 || r == 0x7f {
				return Code{}, fmt.Errorf("pairing code carries a control character in the device %s", field)
			}
		}
	}
	if _, err := age.ParseX25519Recipient(c.AgeRecipient); err != nil {
		return Code{}, fmt.Errorf("pairing code carries an invalid age recipient: %w", err)
	}
	if _, err := devicekeys.Fingerprint(c.SigningPublicKey, c.AgeRecipient); err != nil {
		return Code{}, fmt.Errorf("pairing code carries an invalid signing public key: %w", err)
	}
	return c, nil
}
