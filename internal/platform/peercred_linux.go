//go:build linux

package platform

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// PeerCred returns the identity of the process on the other end of a connected
// Unix domain socket, via SO_PEERCRED.
//
// See peercred_darwin.go for the shared fail-CLOSED rationale, which this
// mirrors: any error means the caller refuses the connection. Linux's ucred
// does carry a pid, so PeerIdentity.PID is populated here — it is useful for
// diagnostics only and must never be used for authorization, since a pid can be
// recycled between the check and any later use.
func PeerCred(conn *net.UnixConn) (PeerIdentity, error) {
	if conn == nil {
		return PeerIdentity{}, fmt.Errorf("peercred: nil connection")
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return PeerIdentity{}, fmt.Errorf("peercred: raw connection: %w", err)
	}

	var cred *unix.Ucred
	var credErr error
	// Both error paths matter — see the darwin implementation.
	controlErr := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	})
	if controlErr != nil {
		return PeerIdentity{}, fmt.Errorf("peercred: control: %w", controlErr)
	}
	if credErr != nil {
		return PeerIdentity{}, fmt.Errorf("peercred: getsockopt SO_PEERCRED: %w", credErr)
	}
	if cred == nil {
		return PeerIdentity{}, fmt.Errorf("peercred: no credentials returned")
	}
	return PeerIdentity{UID: cred.Uid, PID: int(cred.Pid)}, nil
}
