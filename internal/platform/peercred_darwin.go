//go:build darwin

package platform

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// PeerCred returns the identity of the process on the other end of a connected
// Unix domain socket, via LOCAL_PEERCRED.
//
// Unlike ProcessAlive — which is deliberately fail-SAFE, treating an ambiguous
// result as "alive" so a live process's lock is never wrongly stolen — this
// call must be treated as fail-CLOSED by its caller: any error at all, on any
// platform, means the connection is refused. Filesystem permissions (a 0700
// parent directory and a 0600 socket) are the outer defense; this check is the
// layer that additionally rejects root, which can open a socket it does not own
// regardless of its mode.
//
// Darwin's xucred carries no pid, so PeerIdentity.PID is always 0 here. Callers
// must not treat a zero PID as an error — it is an unavailable field, not a
// failed lookup.
func PeerCred(conn *net.UnixConn) (PeerIdentity, error) {
	if conn == nil {
		return PeerIdentity{}, fmt.Errorf("peercred: nil connection")
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return PeerIdentity{}, fmt.Errorf("peercred: raw connection: %w", err)
	}

	var cred *unix.Xucred
	var credErr error
	// Both error paths matter: controlErr reports whether the callback ran at
	// all, credErr reports what happened inside it. Dropping either one turns a
	// failed lookup into a silently-trusted connection.
	controlErr := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	})
	if controlErr != nil {
		return PeerIdentity{}, fmt.Errorf("peercred: control: %w", controlErr)
	}
	if credErr != nil {
		return PeerIdentity{}, fmt.Errorf("peercred: getsockopt LOCAL_PEERCRED: %w", credErr)
	}
	if cred == nil {
		return PeerIdentity{}, fmt.Errorf("peercred: no credentials returned")
	}
	return PeerIdentity{UID: cred.Uid}, nil
}
