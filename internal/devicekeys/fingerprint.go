package devicekeys

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"strings"
	"unicode"

	"filippo.io/age"
)

// fingerprintDomain is the domain-separation label baked into every device
// fingerprint. Bumping the version invalidates all previously displayed
// fingerprints, so it must stay pinned across releases.
const fingerprintDomain = "devstrap/device-fp/v1"

// Fingerprint derives a full-strength (256-bit) device fingerprint that binds a
// device's Ed25519 signing public key to its age X25519 recipient. Both inputs
// are canonicalized by parse-then-re-encode so that cosmetically different
// encodings of the same keys (whitespace, non-canonical base64) yield the same
// fingerprint. The digest is SHA-256 over
//
//	"devstrap/device-fp/v1" || 0x00 || canonicalSigning || 0x00 || canonicalRecipient
//
// encoded as unpadded, uppercase, standard-alphabet base32 (exactly 52
// characters for 32 bytes) and returned as 13 dash-separated groups of 4.
//
// This is deliberately a full fingerprint, never a short authentication string:
// the pairing channel is untrusted, and a truncated code would let an attacker
// brute-force a colliding key pair. Compare the whole value out-of-band.
func Fingerprint(signingPublicKey, ageRecipient string) (string, error) {
	if strings.TrimSpace(signingPublicKey) == "" {
		return "", fmt.Errorf("device fingerprint: signing public key is empty")
	}
	if strings.TrimSpace(ageRecipient) == "" {
		return "", fmt.Errorf("device fingerprint: age recipient is empty")
	}
	pub, err := parsePublicSigningKey(signingPublicKey)
	if err != nil {
		return "", fmt.Errorf("device fingerprint: %w", err)
	}
	canonicalSigning := "ed25519:" + base64.StdEncoding.EncodeToString(pub)
	recipient, err := age.ParseX25519Recipient(strings.TrimSpace(ageRecipient))
	if err != nil {
		return "", fmt.Errorf("device fingerprint: parse age recipient: %w", err)
	}
	canonicalRecipient := recipient.String()

	h := sha256.New()
	h.Write([]byte(fingerprintDomain))
	h.Write([]byte{0})
	h.Write([]byte(canonicalSigning))
	h.Write([]byte{0})
	h.Write([]byte(canonicalRecipient))
	digest := h.Sum(nil)

	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest)
	return groupFingerprint(encoded), nil
}

// groupFingerprint splits an already-encoded fingerprint into dash-separated
// groups of four characters for readable out-of-band comparison.
func groupFingerprint(s string) string {
	var groups []string
	for i := 0; i < len(s); i += 4 {
		end := i + 4
		if end > len(s) {
			end = len(s)
		}
		groups = append(groups, s[i:end])
	}
	return strings.Join(groups, "-")
}

// NormalizeFingerprint strips grouping dashes and whitespace and uppercases the
// remainder so fingerprints copied with varied formatting compare equal.
func NormalizeFingerprint(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '-' || unicode.IsSpace(r) {
			continue
		}
		b.WriteRune(unicode.ToUpper(r))
	}
	return b.String()
}

// FingerprintEqual reports whether two fingerprints are equal after
// normalization, using a constant-time comparison to avoid leaking how much of
// a mistyped value matched.
func FingerprintEqual(a, b string) bool {
	na := NormalizeFingerprint(a)
	nb := NormalizeFingerprint(b)
	if len(na) != len(nb) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(na), []byte(nb)) == 1
}
