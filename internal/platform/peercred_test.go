//go:build darwin || linux

package platform

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// TestPeerCredReportsCallingUID exercises the real syscall against a real
// socket. It can only assert the same-uid case — the test process cannot
// connect as another user without root — so the refusal logic that consumes
// this identity is unit-tested separately (see internal/daemon's
// TestAuthorizePeer, which covers the root and mismatched-uid cases).
func TestPeerCredReportsCallingUID(t *testing.T) {
	// os.MkdirTemp with a short prefix rather than t.TempDir(): a Unix socket
	// address is capped at 104 bytes on darwin, and t.TempDir() embeds the test
	// name.
	dir, err := os.MkdirTemp("", "pc")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s.sock")

	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()

	accepted := make(chan *net.UnixConn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, acceptErrInner := listener.Accept()
		if acceptErrInner != nil {
			acceptErr <- acceptErrInner
			return
		}
		unixConn, ok := conn.(*net.UnixConn)
		if !ok {
			acceptErr <- errNotUnix
			return
		}
		accepted <- unixConn
	}()

	client, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = client.Close() }()

	select {
	case err := <-acceptErr:
		t.Fatalf("accept: %v", err)
	case conn := <-accepted:
		defer func() { _ = conn.Close() }()
		identity, err := PeerCred(conn)
		if err != nil {
			t.Fatalf("PeerCred: %v", err)
		}
		//nolint:gosec // os.Getuid() is a uid; the conversion is not a truncation risk.
		if want := uint32(os.Getuid()); identity.UID != want {
			t.Fatalf("uid = %d, want %d", identity.UID, want)
		}
	}
}

// TestPeerCredRejectsNonUnixConn pins the fail-closed contract: anything that is
// not a connected Unix socket must produce an error, never a zero-valued
// identity a caller could mistake for uid 0 or for success.
func TestPeerCredRejectsNonUnixConn(t *testing.T) {
	if _, err := PeerCred(nil); err == nil {
		t.Fatal("PeerCred(nil) succeeded, want an error")
	}
}

// errNotUnix is only used by the accept goroutine above.
var errNotUnix = errors.New("accepted connection is not a unix socket")
