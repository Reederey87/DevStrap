// ack.go is the wire format for signed per-device sync acknowledgements
// (P4-SYNC-06, P6-HUB-04 completion). After a fully-clean sync cycle a device
// publishes a small signed marker to workspaces/<ws>/meta/acks/<device_id>.json
// stating: the per-origin-device transport cursor it has consumed, the push
// watermark its own stream has reached, and the HLC watermark at or below which
// it has applied every event from every other device. A compactor reads the
// acks of all approved peers and takes the MINIMUM HLC watermark as the safe
// floor for tombstone garbage collection (GCTombstones).
//
// The safety argument (the "tombstone-safety clock"): an ack is written only
// after a clean cycle in which the push watermark reached this device's local
// max Seq, so every event this device mints LATER carries an HLC strictly above
// its acked watermark. A tombstone whose delete-HLC is below the MINIMUM acked
// watermark can therefore never be resurrected — no device can still produce an
// add below that HLC, and every device has already consumed the delete (a clean
// cycle consumes the whole hub log). A later add above the floor is a
// legitimate restore, not a resurrection.
//
// A withheld or stale ack can only DELAY tombstone GC (an availability effect,
// never an integrity one): a missing peer ack skips GC entirely, and a stale
// ack only lowers the min. Ack forgery requires an approved device's private
// signing key — a revoked/lost/unknown device's ack is ignored, so it can
// neither pin nor advance the floor.
package sync

import (
	"encoding/json"
	"fmt"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
)

// Ack-marker versions. v1 is the original P4-SYNC-06 marker. v2 (P4-SYNC-05)
// adds FoldedHash, the running fold over this device's own stream at
// PushedThroughSeq — the SIGNED PER-DEVICE HEAD a pulling peer checks its
// received prefix against to detect a hub withholding this device's newest
// events. New markers are always written at v2; verification accepts v1 (a
// not-yet-upgraded peer's marker, verified without the fold) then v2, mirroring
// the event-signature v1/v2 fallback and retentionManifestVersionOK — so a
// rolling upgrade only degrades omission detection for that peer to fail-safe,
// never rejects the marker outright.
const (
	ackVersionV1 = 1
	ackVersionV2 = 2
	// ackVersion is the version new markers are written at.
	ackVersion = ackVersionV2
)

// AckMarker is one device's signed sync acknowledgement (P4-SYNC-06). Fields
// are declared in alphabetical json-tag order so the marshaled document matches
// the canonical signature payload field-for-field (Go marshals structs in
// declaration order and maps with sorted keys), mirroring RetentionManifest.
type AckMarker struct {
	// Cursor is the per-origin-device transport cursor this device has pulled
	// AND consumed (device_id -> highest contiguous Seq). Pull rows only; the
	// push watermark is carried separately by PushedThroughSeq.
	Cursor map[string]int64 `json:"cursor"`
	// DeviceID is the acking device; it must equal the object key's device id.
	DeviceID string `json:"device_id"`
	// FoldedHash is the P4-SYNC-05 signed per-device head: the running fold
	// (internal/fold) over THIS device's own event stream at PushedThroughSeq.
	// Empty on a v1 marker or when this device cannot establish a fold seed.
	FoldedHash string `json:"folded_hash,omitempty"`
	// HLCWatermark is this device's current HLC clock (state.CurrentHLC): after a
	// clean cycle it is at or above every event HLC the device has applied, so
	// everything at or below it from other devices has been consumed here. It is
	// the value a compactor mins over to floor tombstone GC.
	HLCWatermark int64 `json:"hlc_watermark"`
	// ProducedAt is the HLC at which this marker was produced. In v1 it equals
	// HLCWatermark (both are the current non-minting HLC read); it is kept
	// distinct so a future minted-timestamp variant does not change the wire tag.
	ProducedAt int64 `json:"produced_at_hlc"`
	// PushedThroughSeq is this device's push watermark: the highest local Seq it
	// has published to the hub. A clean-cycle ack has this equal to local max
	// Seq, which is what makes the HLCWatermark a sound tombstone clock.
	PushedThroughSeq int64 `json:"pushed_through_seq"`
	// Sig is the Ed25519 signature over the canonical payload (all fields except
	// Sig), domain AckSignatureDomain.
	Sig string `json:"sig"`
	// V is the marker version.
	V int `json:"v"`
	// WorkspaceID binds the marker to its workspace.
	WorkspaceID string `json:"workspace_id"`
}

// ackSignaturePayload is the canonical signed form of a v1 ack: every field
// except Sig and FoldedHash, declared in alphabetical json-tag order (Go
// marshals the Cursor map with sorted keys, so it is canonical too). Retained
// only so verification can fall back to v1 for a not-yet-upgraded peer's marker.
type ackSignaturePayload struct {
	Cursor           map[string]int64 `json:"cursor"`
	DeviceID         string           `json:"device_id"`
	HLCWatermark     int64            `json:"hlc_watermark"`
	ProducedAt       int64            `json:"produced_at_hlc"`
	PushedThroughSeq int64            `json:"pushed_through_seq"`
	V                int              `json:"v"`
	WorkspaceID      string           `json:"workspace_id"`
}

// ackSignaturePayloadV2 is the v2 signed form: the v1 tuple plus FoldedHash
// (P4-SYNC-05), keys in alphabetical json-tag order.
type ackSignaturePayloadV2 struct {
	Cursor           map[string]int64 `json:"cursor"`
	DeviceID         string           `json:"device_id"`
	FoldedHash       string           `json:"folded_hash"`
	HLCWatermark     int64            `json:"hlc_watermark"`
	ProducedAt       int64            `json:"produced_at_hlc"`
	PushedThroughSeq int64            `json:"pushed_through_seq"`
	V                int              `json:"v"`
	WorkspaceID      string           `json:"workspace_id"`
}

func ackCursorOrEmpty(m AckMarker) map[string]int64 {
	if m.Cursor == nil {
		// Marshal an empty map, never JSON null, so a device with no consumed
		// peer stream signs and verifies over the same bytes.
		return map[string]int64{}
	}
	return m.Cursor
}

// AckSignaturePayload returns the canonical v1 bytes an ack signature covers.
func AckSignaturePayload(m AckMarker) []byte {
	raw, err := json.Marshal(ackSignaturePayload{
		Cursor:           ackCursorOrEmpty(m),
		DeviceID:         m.DeviceID,
		HLCWatermark:     m.HLCWatermark,
		ProducedAt:       m.ProducedAt,
		PushedThroughSeq: m.PushedThroughSeq,
		V:                m.V,
		WorkspaceID:      m.WorkspaceID,
	})
	if err != nil {
		// Marshaling a struct of strings/ints/maps cannot fail.
		panic(err)
	}
	return raw
}

// AckSignaturePayloadV2 returns the canonical v2 bytes an ack signature covers
// (includes FoldedHash).
func AckSignaturePayloadV2(m AckMarker) []byte {
	raw, err := json.Marshal(ackSignaturePayloadV2{
		Cursor:           ackCursorOrEmpty(m),
		DeviceID:         m.DeviceID,
		FoldedHash:       m.FoldedHash,
		HLCWatermark:     m.HLCWatermark,
		ProducedAt:       m.ProducedAt,
		PushedThroughSeq: m.PushedThroughSeq,
		V:                m.V,
		WorkspaceID:      m.WorkspaceID,
	})
	if err != nil {
		// Marshaling a struct of strings/ints/maps cannot fail.
		panic(err)
	}
	return raw
}

// ackSignaturePayloadForVersion selects the canonical bytes for a marker's
// version so Sign and Verify always agree.
func ackSignaturePayloadForVersion(m AckMarker) ([]byte, error) {
	switch m.V {
	case ackVersionV1:
		return AckSignaturePayload(m), nil
	case ackVersionV2:
		return AckSignaturePayloadV2(m), nil
	default:
		return nil, fmt.Errorf("ack marker version %d, want %d or %d", m.V, ackVersionV1, ackVersionV2)
	}
}

// SignAckMarker stamps V (v2) and Sig on the marker using the producing device's
// private signing key (domain devstrap:ack:v1).
func SignAckMarker(m *AckMarker, privateSigningKey string) error {
	m.V = ackVersion
	payload, err := ackSignaturePayloadForVersion(*m)
	if err != nil {
		return fmt.Errorf("sign ack marker: %w", err)
	}
	sig, err := devicekeys.Sign(privateSigningKey, AckSignatureDomain, payload)
	if err != nil {
		return fmt.Errorf("sign ack marker: %w", err)
	}
	m.Sig = sig
	return nil
}

// VerifyAckMarker checks an ack's signature against the claimed producer's
// public signing key. The caller is responsible for the TRUST decision (the key
// must belong to a locally approved device and the DeviceID must match the
// object key) — this only proves the marker bytes were signed by that key.
// Both v1 and v2 markers verify (a rolling upgrade leaves un-upgraded peers
// writing v1); an unknown version is rejected.
func VerifyAckMarker(m AckMarker, publicSigningKey string) error {
	if m.Sig == "" {
		return fmt.Errorf("ack marker is unsigned")
	}
	payload, err := ackSignaturePayloadForVersion(m)
	if err != nil {
		return err
	}
	if err := devicekeys.Verify(publicSigningKey, m.Sig, AckSignatureDomain, payload); err != nil {
		return fmt.Errorf("ack marker signature: %w", err)
	}
	return nil
}

// ParseAckMarker decodes raw ack bytes without verifying the signature.
func ParseAckMarker(raw []byte) (AckMarker, error) {
	var m AckMarker
	if err := json.Unmarshal(raw, &m); err != nil {
		return AckMarker{}, fmt.Errorf("parse ack marker: %w", err)
	}
	return m, nil
}
