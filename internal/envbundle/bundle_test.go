package envbundle

import (
	"bytes"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/Reederey87/DevStrap/internal/envfile"
)

func TestEncryptEncryptsBindingsForRecipient(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	bindings := []envfile.Binding{{Name: "TOKEN", Value: "secret-value", Line: 1}}
	ciphertext, ref, err := Encrypt(bindings, []string{identity.Recipient().String()})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ref, "age_blob:") {
		t.Fatalf("ref = %q, want age_blob", ref)
	}
	if bytes.Contains(ciphertext, []byte("secret-value")) {
		t.Fatalf("ciphertext contains plaintext: %s", ciphertext)
	}
	decoded, err := Decrypt(ciphertext, identity.String())
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Version != 1 || len(decoded.Vars) != 1 || decoded.Vars[0].Value != "secret-value" {
		t.Fatalf("decoded = %+v, want encrypted bindings round-trip", decoded)
	}
}

func TestEncryptRequiresRecipient(t *testing.T) {
	if _, _, err := Encrypt(nil, nil); err == nil {
		t.Fatal("Encrypt succeeded without recipient")
	}
}
