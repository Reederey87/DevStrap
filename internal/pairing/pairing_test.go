package pairing

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
)

func validCode(t *testing.T) Code {
	t.Helper()
	identity, err := devicekeys.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	signing, err := devicekeys.NewSigningIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return Code{
		WorkspaceID:      "ws_0123456789abcdef0123456789abcdef",
		DeviceID:         "dev_0123456789abcdef0123456789abcdef",
		Name:             "laptop",
		OS:               "linux",
		Arch:             "arm64",
		AgeRecipient:     identity.Recipient,
		SigningPublicKey: signing.Public,
	}
}

func encodeWire(t *testing.T, wire wireCode) string {
	t.Helper()
	return encodeWireWithPrefix(t, Prefix, wire)
}

func encodeWireWithPrefix(t *testing.T, prefix string, wire wireCode) string {
	t.Helper()
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(raw)
}

func validWire(t *testing.T) wireCode {
	t.Helper()
	code := validCode(t)
	fp, err := devicekeys.Fingerprint(code.SigningPublicKey, code.AgeRecipient)
	if err != nil {
		t.Fatal(err)
	}
	return wireCode{
		V:    Version,
		WS:   code.WorkspaceID,
		Dev:  code.DeviceID,
		Name: code.Name,
		OS:   code.OS,
		Arch: code.Arch,
		Age:  code.AgeRecipient,
		Sig:  code.SigningPublicKey,
		FP:   fp,
	}
}

// assertCoreEqual compares the seven carried identity fields, ignoring the
// decode-only Version/Fingerprint/HubURI metadata.
func assertCoreEqual(t *testing.T, got, want Code) {
	t.Helper()
	got.Version, got.Fingerprint, got.HubURI = 0, "", ""
	want.Version, want.Fingerprint, want.HubURI = 0, "", ""
	if got != want {
		t.Fatalf("core fields = %#v, want %#v", got, want)
	}
}

func TestEncodeEmitsCurrentVersionWithFingerprint(t *testing.T) {
	input := validCode(t)
	blob, err := Encode(input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(blob, Prefix) {
		t.Fatalf("blob = %q, want %s prefix", blob, Prefix)
	}
	got, err := Decode(blob)
	if err != nil {
		t.Fatal(err)
	}
	assertCoreEqual(t, got, input)
	if got.Version != Version {
		t.Fatalf("Version = %d, want %d", got.Version, Version)
	}
	// The embedded fingerprint must equal a fresh derivation from the same keys.
	want, err := devicekeys.Fingerprint(input.SigningPublicKey, input.AgeRecipient)
	if err != nil {
		t.Fatal(err)
	}
	if !got.HasFingerprint() || !devicekeys.FingerprintEqual(got.Fingerprint, want) {
		t.Fatalf("Fingerprint = %q, want %q", got.Fingerprint, want)
	}
	if got.HasHubURI() {
		t.Fatalf("HubURI = %q, want empty (no hub set)", got.HubURI)
	}
}

func TestEncodeDecodeCarriesHubURI(t *testing.T) {
	input := validCode(t)
	input.HubURI = "git@github.com:me/devstrap-hub.git"
	blob, err := Encode(input)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(blob)
	if err != nil {
		t.Fatal(err)
	}
	if !got.HasHubURI() || got.HubURI != input.HubURI {
		t.Fatalf("HubURI = %q, want %q", got.HubURI, input.HubURI)
	}
}

// TestDecodeV1CodeHasNoEmbeddedFields is the regression guard: a v1
// `devstrap-pair1:` blob (as an old binary emits it — no fp/hub fields) still
// decodes, and the fingerprint/hub-uri report as absent so callers fall back to
// the manual out-of-band flow.
func TestDecodeV1CodeHasNoEmbeddedFields(t *testing.T) {
	code := validCode(t)
	blob := encodeWireWithPrefix(t, "devstrap-pair1:", wireCode{
		V:    1,
		WS:   code.WorkspaceID,
		Dev:  code.DeviceID,
		Name: code.Name,
		OS:   code.OS,
		Arch: code.Arch,
		Age:  code.AgeRecipient,
		Sig:  code.SigningPublicKey,
	})
	got, err := Decode(blob)
	if err != nil {
		t.Fatal(err)
	}
	assertCoreEqual(t, got, code)
	if got.Version != 1 {
		t.Fatalf("Version = %d, want 1", got.Version)
	}
	if got.HasFingerprint() {
		t.Fatalf("v1 code reported a fingerprint %q, want none", got.Fingerprint)
	}
	if got.HasHubURI() {
		t.Fatalf("v1 code reported a hub uri %q, want none", got.HubURI)
	}
}

// TestDecodeAcceptsPaddedPayload: Encode never pads (RawURLEncoding), but the
// pre-v2 decoder tolerated a padded payload (some other tool or
// hand-transcription step could introduce one), and that tolerance must
// survive the v2 rewrite — a regression here would silently break any v1
// pairing code that picked up padding before reaching Decode.
func TestDecodeAcceptsPaddedPayload(t *testing.T) {
	code := validCode(t)
	unpadded := encodeWireWithPrefix(t, "devstrap-pair1:", wireCode{
		V:    1,
		WS:   code.WorkspaceID,
		Dev:  code.DeviceID,
		Name: code.Name,
		OS:   code.OS,
		Arch: code.Arch,
		Age:  code.AgeRecipient,
		Sig:  code.SigningPublicKey,
	})
	padded := unpadded + "=="
	got, err := Decode(padded)
	if err != nil {
		t.Fatalf("Decode(padded) = %v, want success", err)
	}
	assertCoreEqual(t, got, code)
}

func TestDecodeErrorClasses(t *testing.T) {
	base := validWire(t)
	tests := []struct {
		name    string
		blob    string
		wantErr string
	}{
		{
			name:    "missing prefix",
			blob:    "not-a-code",
			wantErr: "not a devstrap pairing code (expected the devstrap-pair2: prefix)",
		},
		{
			name:    "unparseable version",
			blob:    "devstrap-pairX:abc",
			wantErr: "not a devstrap pairing code (expected the devstrap-pair2: prefix)",
		},
		{
			name:    "zero version",
			blob:    encodeWireWithPrefix(t, "devstrap-pair0:", base),
			wantErr: "not a devstrap pairing code (expected the devstrap-pair2: prefix)",
		},
		{
			name:    "too large",
			blob:    Prefix + strings.Repeat("a", 8192),
			wantErr: "pairing code exceeds the 8KB limit",
		},
		{
			name:    "bad base64",
			blob:    Prefix + "%%%%",
			wantErr: "pairing code is not valid base64url:",
		},
		{
			name:    "bad json",
			blob:    Prefix + base64.RawURLEncoding.EncodeToString([]byte("{")),
			wantErr: "pairing code payload is not valid JSON:",
		},
		{
			name: "newer version",
			blob: func() string {
				w := base
				w.V = Version + 1
				return encodeWireWithPrefix(t, "devstrap-pair3:", w)
			}(),
			wantErr: "newer devstrap",
		},
		{
			name: "prefix/field version mismatch",
			blob: func() string {
				w := base
				w.V = 1
				return encodeWire(t, w) // prefix says v2, field says v1
			}(),
			wantErr: "does not match its devstrap-pair2: prefix",
		},
		{
			name: "corrupted embedded fingerprint",
			blob: func() string {
				w := base
				w.FP = "AAAA-BBBB-CCCC-DDDD-EEEE-FFFF-GGGG"
				return encodeWire(t, w)
			}(),
			wantErr: "embedded fingerprint does not match its keys",
		},
		{
			name: "bad workspace id",
			blob: func() string {
				w := base
				w.WS = "ws_bad"
				return encodeWire(t, w)
			}(),
			wantErr: "pairing code carries an invalid workspace id",
		},
		{
			name: "bad device id",
			blob: func() string {
				w := base
				w.Dev = "dev_bad"
				return encodeWire(t, w)
			}(),
			wantErr: "pairing code carries an invalid device id",
		},
		{
			name: "empty name",
			blob: func() string {
				w := base
				w.Name = " \t"
				return encodeWire(t, w)
			}(),
			wantErr: "pairing code carries an empty device name",
		},
		{
			name: "empty os",
			blob: func() string {
				w := base
				w.OS = " "
				return encodeWire(t, w)
			}(),
			wantErr: "pairing code carries an empty device os",
		},
		{
			name: "empty arch",
			blob: func() string {
				w := base
				w.Arch = ""
				return encodeWire(t, w)
			}(),
			wantErr: "pairing code carries an empty device arch",
		},
		{
			name: "bad age recipient",
			blob: func() string {
				w := base
				w.Age = "age1bad"
				w.FP = ""
				return encodeWire(t, w)
			}(),
			wantErr: "pairing code carries an invalid age recipient:",
		},
		{
			name: "bad signing key",
			blob: func() string {
				w := base
				w.Sig = "ed25519:bad"
				w.FP = ""
				return encodeWire(t, w)
			}(),
			wantErr: "pairing code carries an invalid signing public key:",
		},
		{
			name: "control char in hub uri",
			blob: func() string {
				w := base
				w.Hub = "git@github.com:me/hub.git\nrm -rf"
				return encodeWire(t, w)
			}(),
			wantErr: "control character in the device hub uri",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decode(tc.blob)
			if err == nil {
				t.Fatal("Decode returned nil error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Decode error = %q, want containing %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestDecodeAllowsUnknownFields(t *testing.T) {
	code := validCode(t)
	raw, err := json.Marshal(map[string]any{
		"v":      Version,
		"ws":     code.WorkspaceID,
		"dev":    code.DeviceID,
		"name":   code.Name,
		"os":     code.OS,
		"arch":   code.Arch,
		"age":    code.AgeRecipient,
		"sig":    code.SigningPublicKey,
		"future": "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(Prefix + base64.RawURLEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatal(err)
	}
	assertCoreEqual(t, got, code)
}

func TestDecodeTrimsWhitespace(t *testing.T) {
	code := validCode(t)
	blob, err := Encode(code)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode("  " + blob + "\n")
	if err != nil {
		t.Fatal(err)
	}
	assertCoreEqual(t, got, code)
}

// Post-#57 opus review (M2): the blob is an untrusted ingestion path and its
// free-text fields later reach terminal output — control characters must die
// at decode.
func TestDecodeRejectsControlCharacters(t *testing.T) {
	base := validCode(t)
	cases := []struct {
		name   string
		mutate func(*Code)
	}{
		{"newline in name", func(c *Code) { c.Name = "lap\ntop" }},
		{"escape in os", func(c *Code) { c.OS = "lin\x1bux" }},
		{"delete in arch", func(c *Code) { c.Arch = "arm\x7f64" }},
		{"newline in hub uri", func(c *Code) { c.HubURI = "git@h\nub" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code := base
			tc.mutate(&code)
			if _, err := Encode(code); err == nil {
				t.Fatal("Encode accepted a control character, want error")
			}
		})
	}
}
