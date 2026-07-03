package sync

import (
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Reederey87/DevStrap/internal/state"
	"golang.org/x/crypto/chacha20poly1305"
)

// EventEncryptedV2 is the sentinel event Type for an encrypted namespace-map
// envelope (P4-SEC-02 / P4-SEC-07 / P6-SYNC-04). An enc.v2 event preserves the
// carrier fields (ID, DeviceID, Seq, HLC, DeviceSig) so hub ordering, dedup,
// and signature verification are byte-for-byte unchanged, and seals the content
// tuple {Type, PayloadJSON, ContentHash, PrevEventHash} under the Workspace
// Content Key (WCK) for the event's epoch. The hub therefore stores only
// ciphertext for the namespace map; ContentHash/PrevEventHash/CreatedAt are
// cleared and re-stamped on the receiving device after decryption. Unlike
// enc.v1, the AEAD additional data binds the FULL carrier tuple
// (ID, DeviceID, Seq, HLC, kid, epoch), so a hub-side mutation of any carrier
// field is an authentication failure at decrypt time instead of leaking into
// apply-path semantics (P6-SYNC-04).
const EventEncryptedV2 = "enc.v2"

// EventEncryptedV1 is the RETIRED enc.v1 sentinel. enc.v1 bound only
// (ID, epoch) into the AAD, leaving DeviceID/Seq/HLC hub-mutable; the enc.v2
// break (P6-SYNC-04) was taken while every existing hub was a disposable
// spike, so there is no decrypt path for v1 — EncryptedHub.Pull skips v1
// envelopes with a loud "re-found the hub" warning. The constant survives only
// to recognize and reject that legacy traffic.
const EventEncryptedV1 = "enc.v1"

// envelopeVersion is the single supported enc.v2 payload version.
const envelopeVersion = 2

// ErrMissingWorkspaceKey signals that a pulled enc.v2 event references a key
// epoch this device does not hold (after all in-batch grants have been
// ingested). It is returned before the pull cursor advances so the next sync
// retries cleanly once the missing grant arrives.
var ErrMissingWorkspaceKey = errors.New("missing workspace content key for epoch")

// ErrUnknownEnvelopeVersion signals that an enc.v2 event carries an envelope
// version this build cannot decrypt. It is a fail-closed anti-downgrade guard:
// an unknown version is never silently applied.
var ErrUnknownEnvelopeVersion = errors.New("unknown encrypted envelope version")

// ErrPlaintextEventFromHub signals that a non-grant event arrived from the hub
// as plaintext (anti-downgrade). Once envelope encryption is wired, the hub
// event log must contain only enc.v2 envelopes and device.key.granted grants;
// any other plaintext type is a downgrade or an unencrypted regression.
var ErrPlaintextEventFromHub = errors.New("plaintext event from hub (anti-downgrade rejection)")

// wckSize is the XChaCha20-Poly1305 key length (32 bytes). A WCK is exactly one
// symmetric AEAD key per integer epoch.
const wckSize = chacha20poly1305.KeySize

// encryptedEnvelope is the PayloadJSON of an enc.v2 event: the AEAD ciphertext
// (base64(nonce || ciphertext+tag)) plus the (epoch, kid) that selects the WCK.
// KID is the key identity (KIDForWCK) so two colliding keys at the same epoch —
// e.g. a legacy self-mint and the founder's fleet key (P6-SEC-02) — are
// distinguishable without trial decryption. The envelope's kid FIELD remains an
// unauthenticated routing hint (readers treat it as candidate-ordering only,
// see EncryptedHub.Pull), but the SEALING key's kid is bound into the AAD:
// on decrypt the AAD is re-derived with KIDForWCK(candidate), so a ciphertext
// only ever authenticates under the exact key that sealed it, while a hub-side
// relabel of the hint field costs nothing (the relabeled event still decrypts
// under the true key — kept deliberately, a relabel must not become a wedge).
// A kid-less envelope is malformed under enc.v2 (every v2 writer stamps it);
// readers still fall back to trying every held key at the epoch, which the
// AAD makes safe (a wrong candidate fails authentication).
type encryptedEnvelope struct {
	Version int    `json:"v"`
	Epoch   int64  `json:"epoch"`
	KID     string `json:"kid,omitempty"`
	CT      string `json:"ct"`
}

// sealedContent is the plaintext tuple sealed inside the AEAD. Sealing Type and
// ContentHash alongside PayloadJSON closes the per-device activity-timeline and
// low-entropy-confirmation leaks (an observer cannot tell event types or
// content hashes from the envelope).
type sealedContent struct {
	Type          string `json:"type"`
	PayloadJSON   string `json:"payload_json"`
	ContentHash   string `json:"content_hash"`
	PrevEventHash string `json:"prev_event_hash"`
}

// NewWCK generates a fresh 32-byte Workspace Content Key for a new epoch.
func NewWCK() ([]byte, error) {
	key := make([]byte, wckSize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate workspace content key: %w", err)
	}
	return key, nil
}

// KIDForWCK derives the key identity for a WCK: hex(sha256(wck)), the full
// digest as 64 lowercase hex characters (P6-SEC-02). The full digest (not a
// short prefix) makes crafting a second key with a colliding kid a sha256
// collision, so a forged grant can never alias — let alone displace — an
// existing key's custody slot. The kid names a specific key so two
// keys minted independently at the same epoch (a joiner's legacy self-mint and
// the founder's fleet key) coexist in the keyring instead of clobbering, and a
// grant whose carried kid does not match its unwrapped bytes is rejected. The
// kid is derived from the secret key but reveals only a one-way hash — it
// identifies, it does not leak.
func KIDForWCK(wck []byte) string {
	sum := sha256.Sum256(wck)
	return hex.EncodeToString(sum[:])
}

// EncryptEvent seals event's content tuple under the WCK for the given epoch,
// returning an enc.v2 carrier event. The carrier preserves ID, WorkspaceID,
// DeviceID, Seq, HLC, and DeviceSig (so hub ordering and signature verification
// are unchanged) and clears ContentHash, PrevEventHash, and CreatedAt (re-stamped
// on the receiver after DecryptEvent). The AEAD additional data binds the full
// carrier tuple — event.ID, DeviceID, Seq, HLC, the sealing key's kid, and the
// epoch — so mutating ANY of those fields in transit or at rest on the hub is
// detected as an authentication failure (P6-SYNC-04). The envelope also names
// the key by kid (KIDForWCK(wck)) so receivers select the exact key when
// several coexist at the same epoch.
func EncryptEvent(event state.Event, wck []byte, epoch int64) (state.Event, error) {
	if len(wck) != wckSize {
		return state.Event{}, fmt.Errorf("encrypt event %s: wck length = %d, want %d", event.ID, len(wck), wckSize)
	}
	aead, err := aeadFor(wck)
	if err != nil {
		return state.Event{}, err
	}
	plaintext, err := json.Marshal(sealedContent{
		Type:          event.Type,
		PayloadJSON:   event.PayloadJSON,
		ContentHash:   event.ContentHash,
		PrevEventHash: event.PrevEventHash,
	})
	if err != nil {
		return state.Event{}, fmt.Errorf("encrypt event %s: marshal sealed content: %w", event.ID, err)
	}
	nonce := make([]byte, aead.NonceSize()) // 24 bytes for XChaCha20-Poly1305
	if _, err := rand.Read(nonce); err != nil {
		return state.Event{}, fmt.Errorf("encrypt event %s: generate nonce: %w", event.ID, err)
	}
	kid := KIDForWCK(wck)
	ct := aead.Seal(nil, nonce, plaintext, envelopeAAD(event, kid, epoch))
	raw := append(nonce, ct...)
	envelope := encryptedEnvelope{Version: envelopeVersion, Epoch: epoch, KID: kid, CT: base64.StdEncoding.EncodeToString(raw)}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return state.Event{}, fmt.Errorf("encrypt event %s: marshal envelope: %w", event.ID, err)
	}
	return state.Event{
		ID:            event.ID,
		WorkspaceID:   event.WorkspaceID,
		DeviceID:      event.DeviceID,
		Seq:           event.Seq,
		HLC:           event.HLC,
		Type:          EventEncryptedV2,
		PayloadJSON:   string(payload),
		ContentHash:   "",
		PrevEventHash: "",
		DeviceSig:     event.DeviceSig,
		CreatedAt:     "",
	}, nil
}

// DecryptEvent restores an enc.v2 carrier event to its original plaintext form
// using the WCK for the epoch embedded in the envelope. The AEAD additional
// data (ID, DeviceID, Seq, HLC, KIDForWCK(wck), epoch) is re-derived from the
// carrier and the CANDIDATE KEY IN HAND — never from the envelope's
// unauthenticated kid field — so a mutation of any carrier field is an
// authentication failure, while a relabeled kid hint stays harmless (the true
// key still authenticates). The restored event keeps the carrier's
// ID/WorkspaceID/DeviceID/Seq/HLC/DeviceSig and CreatedAt, and restores
// Type/PayloadJSON/ContentHash/PrevEventHash from the sealed tuple so
// insertEvent's content-hash re-derivation and signature verification see the
// exact original bytes.
func DecryptEvent(event state.Event, wck []byte) (state.Event, error) {
	env, err := ParseEncryptedEnvelope(event)
	if err != nil {
		return state.Event{}, err
	}
	if len(wck) != wckSize {
		return state.Event{}, fmt.Errorf("decrypt event %s: wck length = %d, want %d", event.ID, len(wck), wckSize)
	}
	aead, err := aeadFor(wck)
	if err != nil {
		return state.Event{}, err
	}
	raw, err := base64.StdEncoding.DecodeString(env.CT)
	if err != nil {
		return state.Event{}, fmt.Errorf("decrypt event %s: decode ciphertext: %w", event.ID, err)
	}
	nonceSize := aead.NonceSize()
	if len(raw) < nonceSize+aead.Overhead() {
		return state.Event{}, fmt.Errorf("decrypt event %s: ciphertext too short", event.ID)
	}
	nonce, ct := raw[:nonceSize], raw[nonceSize:]
	plaintext, err := aead.Open(nil, nonce, ct, envelopeAAD(event, KIDForWCK(wck), env.Epoch))
	if err != nil {
		return state.Event{}, fmt.Errorf("decrypt event %s: aead open: %w", event.ID, err)
	}
	var sealed sealedContent
	if err := json.Unmarshal(plaintext, &sealed); err != nil {
		return state.Event{}, fmt.Errorf("decrypt event %s: unmarshal sealed content: %w", event.ID, err)
	}
	return state.Event{
		ID:            event.ID,
		WorkspaceID:   event.WorkspaceID,
		DeviceID:      event.DeviceID,
		Seq:           event.Seq,
		HLC:           event.HLC,
		Type:          sealed.Type,
		PayloadJSON:   sealed.PayloadJSON,
		ContentHash:   sealed.ContentHash,
		PrevEventHash: sealed.PrevEventHash,
		DeviceSig:     event.DeviceSig,
		CreatedAt:     event.CreatedAt,
	}, nil
}

// ParseEncryptedEnvelope decodes an enc.v2 event's envelope without decrypting,
// so the decorator can select the WCK by epoch before calling DecryptEvent. It
// validates the Type sentinel and the envelope version.
func ParseEncryptedEnvelope(event state.Event) (encryptedEnvelope, error) {
	if event.Type != EventEncryptedV2 {
		return encryptedEnvelope{}, fmt.Errorf("%w: event %s has type %q, want %q", ErrPlaintextEventFromHub, event.ID, event.Type, EventEncryptedV2)
	}
	var env encryptedEnvelope
	if err := json.Unmarshal([]byte(event.PayloadJSON), &env); err != nil {
		return encryptedEnvelope{}, fmt.Errorf("parse envelope event %s: %w", event.ID, err)
	}
	if env.Version != envelopeVersion {
		return env, fmt.Errorf("%w: event %s has envelope version %d, want %d", ErrUnknownEnvelopeVersion, event.ID, env.Version, envelopeVersion)
	}
	return env, nil
}

// aeadFor builds an XChaCha20-Poly1305 AEAD from a 32-byte WCK. XChaCha20's
// 192-bit nonce makes random nonces safe (collision probability negligible), so
// no per-key nonce counter is needed.
func aeadFor(wck []byte) (cipher.AEAD, error) {
	aead, err := chacha20poly1305.NewX(wck)
	if err != nil {
		return nil, fmt.Errorf("build xchacha20-poly1305 aead: %w", err)
	}
	return aead, nil
}

// envelopeAAD is the additional authenticated data bound into every enc.v2
// seal: the FULL carrier tuple. Binding ID detects ciphertext substitution
// under another carrier's ID; DeviceID detects re-attribution (which would
// corrupt the (HLC, DeviceID, ID) conflict tiebreak and the per-device chain);
// Seq detects predecessor re-pointing (a Seq=1 rewrite forces the wrong
// prev-hash lookup and a permanent ErrEventHashChain cursor hold — a keyless
// hub-controlled soft-wedge under enc.v1); HLC detects ordering mutation; the
// sealing key's kid and the epoch detect key/epoch stripping or downgrade.
// The AAD protection is possession-based (anyone holding the WCK can verify),
// so unlike signature verification it also covers the pre-enrollment window.
//
// Encoding: u32len(ID)||ID || u32len(DeviceID)||DeviceID || u32len(kid)||kid
// || u64(Seq) || u64(HLC) || u64(epoch), all big-endian. Length-prefixing the
// variable-length strings makes the encoding injective (no concatenation
// ambiguity between adjacent fields).
func envelopeAAD(event state.Event, kid string, epoch int64) []byte {
	aad := make([]byte, 0, 12+len(event.ID)+len(event.DeviceID)+len(kid)+24)
	aad = appendLenPrefixed(aad, event.ID)
	aad = appendLenPrefixed(aad, event.DeviceID)
	aad = appendLenPrefixed(aad, kid)
	var nb [8]byte
	// Seq/HLC/epoch are never negative (Seq >= 0, HLC > 0 enforced on apply,
	// epoch >= 1 bootstrapped at init), so the int64→uint64 casts cannot wrap.
	binary.BigEndian.PutUint64(nb[:], uint64(event.Seq)) //nolint:gosec // G115: seq is never negative
	aad = append(aad, nb[:]...)
	binary.BigEndian.PutUint64(nb[:], uint64(event.HLC)) //nolint:gosec // G115: hlc is always positive
	aad = append(aad, nb[:]...)
	binary.BigEndian.PutUint64(nb[:], uint64(epoch)) //nolint:gosec // G115: epoch is always positive
	aad = append(aad, nb[:]...)
	return aad
}

// appendLenPrefixed appends a big-endian uint32 length followed by the string
// bytes, keeping the AAD encoding injective for variable-length fields.
func appendLenPrefixed(dst []byte, s string) []byte {
	var lb [4]byte
	binary.BigEndian.PutUint32(lb[:], uint32(len(s))) //nolint:gosec // G115: field lengths are far below 4GiB
	dst = append(dst, lb[:]...)
	return append(dst, s...)
}
