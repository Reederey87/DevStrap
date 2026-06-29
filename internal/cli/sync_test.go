package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// TestVerifyBlobContentHash (SEC-03): a blob fetched from the hub must hash to
// the sha256 embedded in its signed age_blob:<sha256> ref. A mismatch (hub
// substitution/tampering) is rejected; a missing or malformed hash is rejected.
func TestVerifyBlobContentHash(t *testing.T) {
	payload := []byte("encrypted-blob-content")
	sum := sha256.Sum256(payload)
	hash := hex.EncodeToString(sum[:])
	ref := "age_blob:" + hash

	if err := verifyBlobContentHash(ref, payload); err != nil {
		t.Fatalf("matching hash: unexpected error %v", err)
	}
	if err := verifyBlobContentHash(ref, []byte("tampered-by-hub")); err == nil {
		t.Fatal("mismatched hash: want error, got nil (SEC-03 tamper detection)")
	}
	if err := verifyBlobContentHash("age_blob:", payload); err == nil {
		t.Fatal("empty hash: want error, got nil")
	}
	if err := verifyBlobContentHash("not-a-blob-ref", payload); err == nil {
		t.Fatal("malformed ref: want error, got nil")
	}
}
