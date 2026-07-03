package id

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// New returns a stable DevStrap-style identifier with a short type prefix.
func New(prefix string) (string, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return "", fmt.Errorf("id prefix must not be empty")
	}
	if strings.ContainsAny(prefix, "_/\\ \t\r\n") {
		return "", fmt.Errorf("id prefix %q contains invalid characters", prefix)
	}
	u, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("create uuidv7: %w", err)
	}
	return prefix + "_" + strings.ReplaceAll(u.String(), "-", ""), nil
}

// Valid reports whether value is a canonical identifier as produced by New for
// the given prefix: "<prefix>_" followed by exactly 32 lowercase hex digits.
// It is intentionally strict — user-supplied ids (e.g. `init --workspace-id`)
// must match the minted shape byte-for-byte so a truncated or re-cased paste
// is rejected before it becomes a divergent hub prefix (P4-SEC-07 pairing).
func Valid(prefix, value string) bool {
	rest, ok := strings.CutPrefix(value, prefix+"_")
	if !ok || len(rest) != 32 {
		return false
	}
	for _, c := range rest {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
