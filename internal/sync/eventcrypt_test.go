package sync

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
)

func TestNewWCKLength(t *testing.T) {
	key, err := NewWCK()
	if err != nil {
		t.Fatalf("NewWCK: %v", err)
	}
	if len(key) != wckSize {
		t.Fatalf("NewWCK length = %d, want %d", len(key), wckSize)
	}
	// Two generations are distinct (randomness is live).
	other, _ := NewWCK()
	if string(key) == string(other) {
		t.Fatalf("NewWCK returned identical keys")
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	wck, _ := NewWCK()
	const epoch = int64(1)
	original := state.Event{
		ID:            "evt_01jzroundtrip",
		WorkspaceID:   "ws_01jz",
		DeviceID:      "dev_a",
		Seq:           7,
		HLC:           115763879690240001,
		Type:          EventProjectAdded,
		PayloadJSON:   `{"path":"work/nclh/foc-models","remote_url":"git@github.com:org/foc-models.git","remote_key":"github.com/org/foc-models"}`,
		ContentHash:   state.ContentHash(`{"path":"work/nclh/foc-models"}`),
		PrevEventHash: "sha256:prev",
		DeviceSig:     "ed25519:sig",
		CreatedAt:     "2026-06-30T12:00:00.000000000Z",
	}

	enc, err := EncryptEvent(original, wck, epoch)
	if err != nil {
		t.Fatalf("EncryptEvent: %v", err)
	}
	if enc.Type != EventEncryptedV2 {
		t.Errorf("encrypted Type = %q, want %q", enc.Type, EventEncryptedV2)
	}
	if enc.ContentHash != "" || enc.PrevEventHash != "" || enc.CreatedAt != "" {
		t.Errorf("encrypted carrier must clear ContentHash/PrevEventHash/CreatedAt, got %q/%q/%q", enc.ContentHash, enc.PrevEventHash, enc.CreatedAt)
	}
	// Carrier preserves the ordering/signature fields.
	if enc.ID != original.ID || enc.DeviceID != original.DeviceID || enc.Seq != original.Seq || enc.HLC != original.HLC || enc.DeviceSig != original.DeviceSig {
		t.Errorf("encrypted carrier changed ordering/signature fields")
	}
	// PayloadJSON must not leak plaintext paths/remotes.
	if containsPlaintextLeak(enc.PayloadJSON, "work/nclh/foc-models") || containsPlaintextLeak(enc.PayloadJSON, "github.com/org/foc-models") {
		t.Errorf("encrypted PayloadJSON leaked plaintext: %s", enc.PayloadJSON)
	}

	restored, err := DecryptEvent(enc, wck)
	if err != nil {
		t.Fatalf("DecryptEvent: %v", err)
	}
	if restored.Type != original.Type || restored.PayloadJSON != original.PayloadJSON || restored.ContentHash != original.ContentHash || restored.PrevEventHash != original.PrevEventHash {
		t.Errorf("round-trip did not restore sealed content\ngot  %+v\nwant %+v", restored, original)
	}
	if restored.ID != original.ID || restored.DeviceID != original.DeviceID || restored.Seq != original.Seq || restored.HLC != original.HLC || restored.DeviceSig != original.DeviceSig {
		t.Errorf("round-trip changed carrier fields")
	}
}

// TestEncryptDecryptSignaturePayloadPreserved proves the restored event still
// passes EventSignaturePayload verification: the Ed25519 signature covers
// {ContentHash, HLC, ID, PayloadJSON, PrevEventHash, Type}, and the decrypted
// event reproduces those exact bytes, so DeviceSig still verifies.
func TestEncryptDecryptSignaturePayloadPreserved(t *testing.T) {
	wck, _ := NewWCK()
	original := state.Event{
		ID:            "evt_01jzsig",
		DeviceID:      "dev_a",
		HLC:           42,
		Type:          EventProjectRenamed,
		PayloadJSON:   `{"old_path":"work/a","new_path":"work/b"}`,
		ContentHash:   state.ContentHash(`{"old_path":"work/a","new_path":"work/b"}`),
		PrevEventHash: "sha256:p",
		DeviceSig:     "ed25519:sig",
	}
	enc, err := EncryptEvent(original, wck, 1)
	if err != nil {
		t.Fatalf("EncryptEvent: %v", err)
	}
	restored, err := DecryptEvent(enc, wck)
	if err != nil {
		t.Fatalf("DecryptEvent: %v", err)
	}
	want := state.EventSignaturePayload(original)
	got := state.EventSignaturePayload(restored)
	if string(got) != string(want) {
		t.Errorf("signature payload changed after round-trip\ngot  %s\nwant %s", got, want)
	}
}

func TestDecryptWrongKeyFails(t *testing.T) {
	wck, _ := NewWCK()
	other, _ := NewWCK()
	enc, err := EncryptEvent(state.Event{ID: "evt_wrongkey", Type: EventProjectAdded, PayloadJSON: `{}`, ContentHash: state.ContentHash(`{}`)}, wck, 1)
	if err != nil {
		t.Fatalf("EncryptEvent: %v", err)
	}
	if _, err := DecryptEvent(enc, other); err == nil {
		t.Fatalf("DecryptEvent with wrong key unexpectedly succeeded")
	}
}

// TestDecryptMutatedCarrierFails is the core enc.v2 tamper proof (P6-SYNC-04):
// every carrier field an untrusted hub could rewrite on a stored object — ID,
// DeviceID, Seq, HLC — is bound into the AEAD AAD, so any mutation is an
// authentication failure at decrypt time, never an apply-path wedge
// (under enc.v1 a Seq rewrite reached validatePrevEventHash and held the
// cursor forever — a keyless hub-controlled soft-wedge).
func TestDecryptMutatedCarrierFails(t *testing.T) {
	wck, _ := NewWCK()
	original := state.Event{
		ID:          "evt_orig_id",
		DeviceID:    "dev_orig",
		Seq:         7,
		HLC:         115763879690240001,
		Type:        EventProjectAdded,
		PayloadJSON: `{}`,
		ContentHash: state.ContentHash(`{}`),
	}
	tests := []struct {
		name   string
		mutate func(e *state.Event)
	}{
		{"mutated ID", func(e *state.Event) { e.ID = "evt_tampered_id" }},
		{"mutated DeviceID", func(e *state.Event) { e.DeviceID = "dev_attacker" }},
		{"mutated Seq", func(e *state.Event) { e.Seq = 1 }},
		{"mutated HLC", func(e *state.Event) { e.HLC = original.HLC + 1 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enc, err := EncryptEvent(original, wck, 1)
			if err != nil {
				t.Fatalf("EncryptEvent: %v", err)
			}
			tt.mutate(&enc)
			if _, err := DecryptEvent(enc, wck); err == nil {
				t.Fatalf("DecryptEvent with %s unexpectedly succeeded", tt.name)
			}
		})
	}
	// Control: the untampered carrier still decrypts.
	enc, err := EncryptEvent(original, wck, 1)
	if err != nil {
		t.Fatalf("EncryptEvent: %v", err)
	}
	if _, err := DecryptEvent(enc, wck); err != nil {
		t.Fatalf("untampered DecryptEvent: %v", err)
	}
}

// TestDecryptRelabeledKIDHintStillDecrypts pins the deliberate asymmetry: the
// envelope's kid FIELD is a routing hint outside the AAD (the AAD binds the
// sealing key's kid derived from the candidate in hand), so a hub-side relabel
// of the hint cannot turn a decryptable event into a permanent failure.
func TestDecryptRelabeledKIDHintStillDecrypts(t *testing.T) {
	wck, _ := NewWCK()
	enc, err := EncryptEvent(state.Event{ID: "evt_kidhint", Type: EventProjectAdded, PayloadJSON: `{}`, ContentHash: state.ContentHash(`{}`)}, wck, 1)
	if err != nil {
		t.Fatalf("EncryptEvent: %v", err)
	}
	var env encryptedEnvelope
	if err := json.Unmarshal([]byte(enc.PayloadJSON), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	env.KID = "0000000000000000000000000000000000000000000000000000000000000000"
	raw, _ := json.Marshal(env)
	enc.PayloadJSON = string(raw)
	if _, err := DecryptEvent(enc, wck); err != nil {
		t.Fatalf("DecryptEvent with relabeled kid hint: %v", err)
	}
}

func TestParseEncryptedEnvelopeUnknownVersion(t *testing.T) {
	wck, _ := NewWCK()
	enc, err := EncryptEvent(state.Event{ID: "evt_ver", Type: EventProjectAdded, PayloadJSON: `{}`, ContentHash: state.ContentHash(`{}`)}, wck, 1)
	if err != nil {
		t.Fatalf("EncryptEvent: %v", err)
	}
	// Forge an envelope with the retired v1 version: rejected fail-closed, so
	// a downgrade to the weaker (ID, epoch)-only AAD can never be decrypted.
	var env encryptedEnvelope
	if err := json.Unmarshal([]byte(enc.PayloadJSON), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	env.Version = 1
	raw, _ := json.Marshal(env)
	enc.PayloadJSON = string(raw)
	_, err = ParseEncryptedEnvelope(enc)
	if !errors.Is(err, ErrUnknownEnvelopeVersion) {
		t.Fatalf("ParseEncryptedEnvelope: got %v, want ErrUnknownEnvelopeVersion", err)
	}
	// DecryptEvent must also reject the unknown version.
	if _, err := DecryptEvent(enc, wck); !errors.Is(err, ErrUnknownEnvelopeVersion) {
		t.Fatalf("DecryptEvent: got %v, want ErrUnknownEnvelopeVersion", err)
	}
}

func TestParseEncryptedEnvelopeRejectsPlaintext(t *testing.T) {
	plain := state.Event{ID: "evt_plain", Type: EventProjectAdded, PayloadJSON: `{}`}
	_, err := ParseEncryptedEnvelope(plain)
	if !errors.Is(err, ErrPlaintextEventFromHub) {
		t.Fatalf("ParseEncryptedEnvelope on plaintext: got %v, want ErrPlaintextEventFromHub", err)
	}
}

func TestDecryptShortCiphertextFails(t *testing.T) {
	wck, _ := NewWCK()
	env := encryptedEnvelope{Version: envelopeVersion, Epoch: 1, CT: base64.StdEncoding.EncodeToString([]byte("short"))}
	raw, _ := json.Marshal(env)
	enc := state.Event{ID: "evt_short", Type: EventEncryptedV2, PayloadJSON: string(raw)}
	if _, err := DecryptEvent(enc, wck); err == nil {
		t.Fatalf("DecryptEvent with short ciphertext unexpectedly succeeded")
	}
}

func TestEncryptWrongWCKLengthFails(t *testing.T) {
	if _, err := EncryptEvent(state.Event{ID: "evt_x", Type: EventProjectAdded}, []byte("tooshort"), 1); err == nil {
		t.Fatalf("EncryptEvent with short WCK unexpectedly succeeded")
	}
}

func containsPlaintextLeak(payload, needle string) bool {
	if needle == "" {
		return false
	}
	// The envelope payload is JSON; a literal plaintext substring would only
	// appear if the sealed content leaked unencrypted.
	return stringContains(payload, needle)
}

func stringContains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// TestKIDForWCK pins the key-identity derivation (P6-SEC-02): deterministic,
// the full 64-lowercase-hex-char digest, and distinct for distinct keys.
func TestKIDForWCK(t *testing.T) {
	wck, err := NewWCK()
	if err != nil {
		t.Fatal(err)
	}
	kid := KIDForWCK(wck)
	if len(kid) != 64 {
		t.Fatalf("kid length = %d, want 64", len(kid))
	}
	for _, c := range kid {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Fatalf("kid %q contains non-lowercase-hex character %q", kid, c)
		}
	}
	if KIDForWCK(wck) != kid {
		t.Fatal("KIDForWCK is not deterministic")
	}
	other, err := NewWCK()
	if err != nil {
		t.Fatal(err)
	}
	if KIDForWCK(other) == kid {
		t.Fatal("distinct WCKs produced the same kid")
	}
}

// TestEnvelopeCarriesKID proves EncryptEvent names its key in the envelope so
// receivers holding several keys at an epoch select the right one without
// trial decryption.
func TestEnvelopeCarriesKID(t *testing.T) {
	wck, err := NewWCK()
	if err != nil {
		t.Fatal(err)
	}
	enc, err := EncryptEvent(state.Event{ID: "evt_kid", Type: EventProjectAdded, PayloadJSON: `{}`, ContentHash: state.ContentHash(`{}`)}, wck, 1)
	if err != nil {
		t.Fatal(err)
	}
	env, err := ParseEncryptedEnvelope(enc)
	if err != nil {
		t.Fatal(err)
	}
	if env.KID != KIDForWCK(wck) {
		t.Fatalf("envelope kid = %q, want %q", env.KID, KIDForWCK(wck))
	}
}
