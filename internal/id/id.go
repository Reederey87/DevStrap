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
