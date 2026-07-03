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
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	return Prefix + base64.RawURLEncoding.EncodeToString(raw)
}

func validWire(t *testing.T) wireCode {
	t.Helper()
	code := validCode(t)
	return wireCode{
		V:    Version,
		WS:   code.WorkspaceID,
		Dev:  code.DeviceID,
		Name: code.Name,
		OS:   code.OS,
		Arch: code.Arch,
		Age:  code.AgeRecipient,
		Sig:  code.SigningPublicKey,
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	input := validCode(t)
	blob, err := Encode(input)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(blob)
	if err != nil {
		t.Fatal(err)
	}
	if got != input {
		t.Fatalf("Decode(Encode(input)) = %#v, want %#v", got, input)
	}
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
			wantErr: "not a devstrap pairing code (expected the devstrap-pair1: prefix)",
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
				return encodeWire(t, w)
			}(),
			wantErr: "newer devstrap",
		},
		{
			name: "missing version",
			blob: func() string {
				w := base
				w.V = 0
				return encodeWire(t, w)
			}(),
			wantErr: "pairing code has no version",
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
				return encodeWire(t, w)
			}(),
			wantErr: "pairing code carries an invalid age recipient:",
		},
		{
			name: "bad signing key",
			blob: func() string {
				w := base
				w.Sig = "ed25519:bad"
				return encodeWire(t, w)
			}(),
			wantErr: "pairing code carries an invalid signing public key:",
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
	if got != code {
		t.Fatalf("Decode with unknown field = %#v, want %#v", got, code)
	}
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
	if got != code {
		t.Fatalf("Decode whitespace = %#v, want %#v", got, code)
	}
}
