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

// ackVersion is the single supported ack-marker version.
const ackVersion = 1

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

// ackSignaturePayload is the canonical signed form of an ack: every field except
// Sig, declared in alphabetical json-tag order (Go marshals the Cursor map with
// sorted keys, so it is canonical too).
type ackSignaturePayload struct {
	Cursor           map[string]int64 `json:"cursor"`
	DeviceID         string           `json:"device_id"`
	HLCWatermark     int64            `json:"hlc_watermark"`
	ProducedAt       int64            `json:"produced_at_hlc"`
	PushedThroughSeq int64            `json:"pushed_through_seq"`
	V                int              `json:"v"`
	WorkspaceID      string           `json:"workspace_id"`
}

// AckSignaturePayload returns the canonical bytes an ack signature covers.
func AckSignaturePayload(m AckMarker) []byte {
	cursor := m.Cursor
	if cursor == nil {
		// Marshal an empty map, never JSON null, so a device with no consumed
		// peer stream signs and verifies over the same bytes.
		cursor = map[string]int64{}
	}
	raw, err := json.Marshal(ackSignaturePayload{
		Cursor:           cursor,
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

// SignAckMarker stamps V and Sig on the marker using the producing device's
// private signing key (domain devstrap:ack:v1).
func SignAckMarker(m *AckMarker, privateSigningKey string) error {
	m.V = ackVersion
	sig, err := devicekeys.Sign(privateSigningKey, AckSignatureDomain, AckSignaturePayload(*m))
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
func VerifyAckMarker(m AckMarker, publicSigningKey string) error {
	if m.V != ackVersion {
		return fmt.Errorf("ack marker version %d, want %d", m.V, ackVersion)
	}
	if m.Sig == "" {
		return fmt.Errorf("ack marker is unsigned")
	}
	if err := devicekeys.Verify(publicSigningKey, m.Sig, AckSignatureDomain, AckSignaturePayload(m)); err != nil {
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
