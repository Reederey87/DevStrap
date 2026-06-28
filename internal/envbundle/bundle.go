package envbundle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"

	"filippo.io/age"
	"github.com/Reederey87/DevStrap/internal/envfile"
)

type Plaintext struct {
	Version int               `json:"version"`
	Vars    []envfile.Binding `json:"vars"`
}

func Encrypt(bindings []envfile.Binding, recipients []string) ([]byte, string, error) {
	ageRecipients := make([]age.Recipient, 0, len(recipients))
	for _, raw := range recipients {
		recipient, err := age.ParseX25519Recipient(raw)
		if err != nil {
			return nil, "", fmt.Errorf("parse age recipient: %w", err)
		}
		ageRecipients = append(ageRecipients, recipient)
	}
	if len(ageRecipients) == 0 {
		return nil, "", fmt.Errorf("at least one age recipient is required")
	}
	plaintext, err := json.Marshal(Plaintext{Version: 1, Vars: bindings})
	if err != nil {
		return nil, "", fmt.Errorf("marshal env bundle: %w", err)
	}
	var buf bytes.Buffer
	writer, err := age.Encrypt(&buf, ageRecipients...)
	if err != nil {
		return nil, "", fmt.Errorf("encrypt env bundle: %w", err)
	}
	if _, err := writer.Write(plaintext); err != nil {
		return nil, "", fmt.Errorf("write env bundle: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("close env bundle: %w", err)
	}
	ciphertext := buf.Bytes()
	sum := sha256.Sum256(ciphertext)
	return ciphertext, "age_blob:" + hex.EncodeToString(sum[:]), nil
}

func Decrypt(ciphertext []byte, identity string) (Plaintext, error) {
	ageIdentity, err := age.ParseX25519Identity(identity)
	if err != nil {
		return Plaintext{}, fmt.Errorf("parse age identity: %w", err)
	}
	reader, err := age.Decrypt(bytes.NewReader(ciphertext), ageIdentity)
	if err != nil {
		return Plaintext{}, fmt.Errorf("decrypt env bundle: %w", err)
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		return Plaintext{}, fmt.Errorf("read env bundle plaintext: %w", err)
	}
	var plaintext Plaintext
	if err := json.Unmarshal(raw, &plaintext); err != nil {
		return Plaintext{}, fmt.Errorf("decode env bundle: %w", err)
	}
	if plaintext.Version != 1 {
		return Plaintext{}, fmt.Errorf("unsupported env bundle version %d", plaintext.Version)
	}
	return plaintext, nil
}

// Rewrap decrypts an age-encrypted blob with the local identity and re-encrypts
// it to the given recipient set (HUB-04). It is generic: it operates at the age
// layer and works for any age-encrypted blob (env or draft), because age has no
// native revocation — re-encryption to the reduced recipient set is the only way
// to limit a revoked device's future access. Returns the new ciphertext and its
// content-addressed age_blob:<sha256> ref.
func Rewrap(ciphertext []byte, identity string, recipients []string) ([]byte, string, error) {
	ageIdentity, err := age.ParseX25519Identity(identity)
	if err != nil {
		return nil, "", fmt.Errorf("parse age identity: %w", err)
	}
	reader, err := age.Decrypt(bytes.NewReader(ciphertext), ageIdentity)
	if err != nil {
		return nil, "", fmt.Errorf("decrypt blob for rewrap: %w", err)
	}
	plaintext, err := io.ReadAll(reader)
	if err != nil {
		return nil, "", fmt.Errorf("read blob plaintext for rewrap: %w", err)
	}
	ageRecipients := make([]age.Recipient, 0, len(recipients))
	for _, raw := range recipients {
		recipient, err := age.ParseX25519Recipient(raw)
		if err != nil {
			return nil, "", fmt.Errorf("parse age recipient: %w", err)
		}
		ageRecipients = append(ageRecipients, recipient)
	}
	if len(ageRecipients) == 0 {
		return nil, "", fmt.Errorf("at least one age recipient is required")
	}
	var buf bytes.Buffer
	writer, err := age.Encrypt(&buf, ageRecipients...)
	if err != nil {
		return nil, "", fmt.Errorf("encrypt blob for rewrap: %w", err)
	}
	if _, err := writer.Write(plaintext); err != nil {
		return nil, "", fmt.Errorf("write blob for rewrap: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("close blob for rewrap: %w", err)
	}
	newCiphertext := buf.Bytes()
	sum := sha256.Sum256(newCiphertext)
	return newCiphertext, "age_blob:" + hex.EncodeToString(sum[:]), nil
}
