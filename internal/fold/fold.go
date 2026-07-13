// Package fold implements the folded running hash chain (P4-SYNC-05): a running
// commitment over one origin device's event stream.
//
// Where prev_event_hash is a POINTER — each event names its predecessor's
// content hash, which detects a dropped or reordered event in the MIDDLE of a
// stream (the successor's pointer no longer matches) — the fold is a running
// COMMITMENT: a single value that commits to the ENTIRE prefix of one device's
// stream up to a seq. That is the missing piece for two attacks the pointer
// chain cannot see:
//
//   - Tail truncation / omission: dropping the NEWEST events leaves the retained
//     prefix internally consistent (nothing points forward), so a pointer chain
//     over events 1..7 cannot tell that events 8..10 exist. The fold plus a
//     device's own SIGNED head (which states "my stream reaches seq N with fold
//     H") lets a pulling peer notice the head commits to a seq beyond what it
//     received.
//   - Equivocation / fork: two divergent histories that both reach seq N produce
//     DIFFERENT folds. A hub that splices a different event into the middle, or a
//     device that signs two different heads at the same seq, is caught because
//     the peer's independently-folded value will not match the signed one.
//
// The construction mirrors Certificate Transparency's signed tree head over a
// hash-chain structure (RFC 9162), minus the Merkle inclusion-proof machinery:
// DevStrap has no third-party auditors and no need for logarithmic-size proofs,
// so a linear running fold with a signed head is sufficient.
package fold

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// Domain separation: the two hashing steps carry distinct fixed labels so a
// seed can never be confused with a step, and both are bound to the workspace
// and origin device so a fold computed for one (workspace, device) stream can
// never collide with, or be replayed into, another.
const (
	seedDomain = "devstrap:devicehead-fold-seed:v1"
	stepDomain = "devstrap:devicehead-fold-step:v1"
)

// State is the 32-byte running fold. It is opaque; use Encode / Decode to move
// it across the store and wire as a "sha256:"-prefixed hex string (matching
// state.ContentHash's convention).
type State [32]byte

// Seed returns the fold at seq 0 for a (workspace, device) stream: the
// deterministic origin the first event folds onto. Binding it to the workspace
// and device ids keeps every device's chain in its own space.
func Seed(workspaceID, deviceID string) State {
	h := sha256.New()
	h.Write([]byte(seedDomain))
	h.Write([]byte{0})
	h.Write([]byte(workspaceID))
	h.Write([]byte{0})
	h.Write([]byte(deviceID))
	var out State
	copy(out[:], h.Sum(nil))
	return out
}

// Step folds one event into the running hash:
//
//	fold_seq = H(stepDomain || fold_{seq-1} || bigendian(seq) || content_hash)
//
// content_hash is the event's already-computed state.ContentHash (a
// "sha256:"-prefixed hex string). Folding the seq in binds each step to its
// position, so a reorder or a substitution at any position changes every
// subsequent fold.
func Step(prev State, seq int64, contentHash string) State {
	h := sha256.New()
	h.Write([]byte(stepDomain))
	h.Write([]byte{0})
	h.Write(prev[:])
	var s [8]byte
	binary.BigEndian.PutUint64(s[:], uint64(seq)) //nolint:gosec // G115: seq is always positive
	h.Write(s[:])
	h.Write([]byte(contentHash))
	var out State
	copy(out[:], h.Sum(nil))
	return out
}

// Encode renders a fold as a "sha256:"-prefixed hex string for storage and the
// ack wire format.
func Encode(f State) string {
	return "sha256:" + hex.EncodeToString(f[:])
}

// Decode parses a "sha256:"-prefixed hex fold string. An empty string decodes
// to the zero fold with ok=false so callers can treat "no recorded fold"
// distinctly from a real value.
func Decode(s string) (State, bool, error) {
	var f State
	if s == "" {
		return f, false, nil
	}
	const prefix = "sha256:"
	if len(s) <= len(prefix) || s[:len(prefix)] != prefix {
		return f, false, fmt.Errorf("fold %q missing sha256: prefix", s)
	}
	raw, err := hex.DecodeString(s[len(prefix):])
	if err != nil {
		return f, false, fmt.Errorf("decode fold %q: %w", s, err)
	}
	if len(raw) != len(f) {
		return f, false, fmt.Errorf("fold %q is %d bytes, want %d", s, len(raw), len(f))
	}
	copy(f[:], raw)
	return f, true, nil
}
