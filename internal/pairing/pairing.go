// Package pairing encodes the one-paste device pairing code (P4-SEC-04 part 2):
// a single copy-paste blob replacing the seven hand-pasted values of the manual
// ceremony. The blob is DELIBERATELY UNAUTHENTICATED - integrity comes from the
// out-of-band fingerprint comparison at approval time (the fingerprint is always
// DERIVED from the carried keys, never carried in the blob, so a tampered blob
// changes the fingerprint and fails the ceremony).
package pairing

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"filippo.io/age"
	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/id"
)

const Prefix = "devstrap-pair1:"
const Version = 1

type wireCode struct {
	V    int    `json:"v"`
	WS   string `json:"ws"`   // workspace id ws_<32 lower hex>
	Dev  string `json:"dev"`  // device id dev_<32 lower hex>
	Name string `json:"name"` // device display name
	OS   string `json:"os"`
	Arch string `json:"arch"`
	Age  string `json:"age"` // age X25519 recipient
	Sig  string `json:"sig"` // ed25519:<base64> signing public key
}

type Code struct {
	WorkspaceID      string
	DeviceID         string
	Name             string
	OS               string
	Arch             string
	AgeRecipient     string
	SigningPublicKey string
}

func Encode(c Code) (string, error) {
	validated, err := validate(c)
	if err != nil {
		return "", err
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
	})
	if err != nil {
		return "", err
	}
	return Prefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

// Decode parses an unauthenticated-by-design pairing blob. Consumers MUST
// derive and confirm the device fingerprint from the returned keys before
// approving the carried device.
func Decode(blob string) (Code, error) {
	blob = strings.TrimSpace(blob)
	if !strings.HasPrefix(blob, Prefix) {
		return Code{}, fmt.Errorf("not a devstrap pairing code (expected the devstrap-pair1: prefix)")
	}
	if len(blob) > 8192 {
		return Code{}, fmt.Errorf("pairing code exceeds the 8KB limit")
	}
	payload := strings.TrimRight(strings.TrimPrefix(blob, Prefix), "=")
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return Code{}, fmt.Errorf("pairing code is not valid base64url: %w", err)
	}
	var wire wireCode
	if err := json.Unmarshal(raw, &wire); err != nil {
		return Code{}, fmt.Errorf("pairing code payload is not valid JSON: %w", err)
	}
	if wire.V > Version {
		return Code{}, fmt.Errorf("pairing code was created by a newer devstrap (v%d); upgrade this binary", wire.V)
	}
	if wire.V < 1 {
		return Code{}, fmt.Errorf("pairing code has no version")
	}
	return validate(Code{
		WorkspaceID:      wire.WS,
		DeviceID:         wire.Dev,
		Name:             wire.Name,
		OS:               wire.OS,
		Arch:             wire.Arch,
		AgeRecipient:     wire.Age,
		SigningPublicKey: wire.Sig,
	})
}

func validate(c Code) (Code, error) {
	c.WorkspaceID = strings.TrimSpace(c.WorkspaceID)
	c.DeviceID = strings.TrimSpace(c.DeviceID)
	c.Name = strings.TrimSpace(c.Name)
	c.OS = strings.TrimSpace(c.OS)
	c.Arch = strings.TrimSpace(c.Arch)
	c.AgeRecipient = strings.TrimSpace(c.AgeRecipient)
	c.SigningPublicKey = strings.TrimSpace(c.SigningPublicKey)

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
	if _, err := age.ParseX25519Recipient(c.AgeRecipient); err != nil {
		return Code{}, fmt.Errorf("pairing code carries an invalid age recipient: %w", err)
	}
	if _, err := devicekeys.Fingerprint(c.SigningPublicKey, c.AgeRecipient); err != nil {
		return Code{}, fmt.Errorf("pairing code carries an invalid signing public key: %w", err)
	}
	return c, nil
}
