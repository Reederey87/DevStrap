package devicekeys

import (
	"strings"
	"testing"
)

// Golden vector: these exact inputs must forever map to this exact fingerprint.
// Regenerating it silently breaks every already-paired device, so the literal
// is pinned rather than recomputed.
const (
	goldenSigning     = "ed25519:bmAFCuJRIDCk0J32Zhx0qMgSQOflaWHrDH0TImfTYSQ="
	goldenRecipient   = "age1cjsjvu9yhtl6up6msera7686z56rysh9wg0uda2wehh02rmr3qzq6c8xck"
	goldenFingerprint = "36X4-I4BU-VCV5-KIGN-V4ZL-27NZ-Y2DO-VPIP-NYFT-UKB4-U4J3-AESR-ITXA"
)

func TestFingerprintGoldenVector(t *testing.T) {
	got, err := Fingerprint(goldenSigning, goldenRecipient)
	if err != nil {
		t.Fatalf("Fingerprint returned error: %v", err)
	}
	if got != goldenFingerprint {
		t.Fatalf("Fingerprint = %q, want %q", got, goldenFingerprint)
	}
	// 52 base32 chars from a 256-bit digest, grouped 13x4 with 12 dashes = 64.
	if len(got) != 64 {
		t.Fatalf("grouped length = %d, want 64", len(got))
	}
	if got := NormalizeFingerprint(got); len(got) != 52 {
		t.Fatalf("normalized length = %d, want 52", len(got))
	}
	if strings.ToUpper(got) != got {
		t.Fatalf("fingerprint not uppercase: %q", got)
	}
}

func TestFingerprintCanonicalizesInputs(t *testing.T) {
	// Surrounding whitespace on either input must not change the fingerprint.
	spaced, err := Fingerprint("  "+goldenSigning+"\n", "\t"+goldenRecipient+"  ")
	if err != nil {
		t.Fatal(err)
	}
	if spaced != goldenFingerprint {
		t.Fatalf("whitespace changed fingerprint: %q != %q", spaced, goldenFingerprint)
	}
}

func TestFingerprintRejectsEmptyOrBadInputs(t *testing.T) {
	cases := []struct {
		name               string
		signing, recipient string
	}{
		{"empty signing", "", goldenRecipient},
		{"empty recipient", goldenSigning, ""},
		{"whitespace signing", "   ", goldenRecipient},
		{"bad signing", "not-a-key", goldenRecipient},
		{"bad recipient", goldenSigning, "age1notvalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Fingerprint(tc.signing, tc.recipient); err == nil {
				t.Fatalf("Fingerprint(%q,%q) = nil error, want error", tc.signing, tc.recipient)
			}
		})
	}
}

// Changing EITHER input must change the fingerprint, proving both keys are bound.
func TestFingerprintBindsBothKeys(t *testing.T) {
	base, err := Fingerprint(goldenSigning, goldenRecipient)
	if err != nil {
		t.Fatal(err)
	}

	otherSigning, err := NewSigningIdentity()
	if err != nil {
		t.Fatal(err)
	}
	changedSigning, err := Fingerprint(otherSigning.Public, goldenRecipient)
	if err != nil {
		t.Fatal(err)
	}
	if changedSigning == base {
		t.Fatal("changing the signing key did not change the fingerprint")
	}

	otherRecipient, err := NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	changedRecipient, err := Fingerprint(goldenSigning, otherRecipient.Recipient)
	if err != nil {
		t.Fatal(err)
	}
	if changedRecipient == base {
		t.Fatal("changing the age recipient did not change the fingerprint")
	}
}

func TestNormalizeFingerprint(t *testing.T) {
	want := NormalizeFingerprint(goldenFingerprint)
	variants := []string{
		goldenFingerprint,
		strings.ToLower(goldenFingerprint),
		strings.ReplaceAll(goldenFingerprint, "-", ""),
		strings.ReplaceAll(goldenFingerprint, "-", " "),
		"  " + goldenFingerprint + "  ",
		strings.ToLower(strings.ReplaceAll(goldenFingerprint, "-", " ")),
	}
	for _, v := range variants {
		if got := NormalizeFingerprint(v); got != want {
			t.Fatalf("NormalizeFingerprint(%q) = %q, want %q", v, got, want)
		}
	}
}

func TestFingerprintEqual(t *testing.T) {
	if !FingerprintEqual(goldenFingerprint, strings.ToLower(goldenFingerprint)) {
		t.Fatal("case difference should compare equal")
	}
	if !FingerprintEqual(goldenFingerprint, strings.ReplaceAll(goldenFingerprint, "-", " ")) {
		t.Fatal("dashes-as-spaces should compare equal")
	}
	if !FingerprintEqual(goldenFingerprint, strings.ReplaceAll(goldenFingerprint, "-", "")) {
		t.Fatal("stripped dashes should compare equal")
	}
	if FingerprintEqual(goldenFingerprint, "AAAA-BBBB") {
		t.Fatal("different-length fingerprints must not compare equal")
	}
	// Flip one character.
	mutated := "46X4" + goldenFingerprint[4:]
	if FingerprintEqual(goldenFingerprint, mutated) {
		t.Fatal("single-character difference must not compare equal")
	}
}
