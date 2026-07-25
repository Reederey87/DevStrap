//go:build !darwin && !linux

package platform

import (
	"fmt"
	"net"
)

// PeerCred is unsupported on this platform.
//
// This returns ErrUnsupported rather than a permissive zero value on purpose:
// the daemon's authorization decision is fail-CLOSED, so a platform where peer
// credentials cannot be read must refuse to serve the socket entirely rather
// than fall back to filesystem permissions alone. That matches how the service
// and sandbox adapters treat unsupported platforms, and it is the opposite of
// ProcessAlive's deliberately fail-SAFE fallback — the two seams are answering
// different questions ("may I let this caller in?" vs "may I steal this lock?").
func PeerCred(_ *net.UnixConn) (PeerIdentity, error) {
	return PeerIdentity{}, fmt.Errorf("peercred: %w", ErrUnsupported)
}
