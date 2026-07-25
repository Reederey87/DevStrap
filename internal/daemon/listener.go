// Package daemon implements DevStrap's local control API: an HTTP+JSON service
// over a Unix domain socket, as specified in spec/13_CLI_DAEMON_API.md.
//
// The package is transport only. It holds no convergence logic and imports no
// command code; callers inject what it needs (see Config). That keeps the
// dependency arrow pointing daemon -> core and never daemon -> cobra, which is
// the property the ARCH2-01 engine-seam finding actually cares about (see
// spec/03).
package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"time"
)

// ErrAlreadyRunning reports that another process is already listening on the
// socket. It is deliberately distinct from a stale-socket condition: a live
// daemon must never be evicted by a second one starting up.
var ErrAlreadyRunning = errors.New("daemon: already running")

// dialProbeTimeout bounds the liveness probe in Listen. It is short because the
// peer is a local process on the same machine: either it accepts effectively
// immediately or it is not there.
const dialProbeTimeout = 250 * time.Millisecond

// Listen creates the daemon's Unix domain socket, taking over a stale socket
// left behind by a crashed process but refusing to displace a live one.
//
// Access control is layered:
//
//   - the parent directory is forced to 0700, which is the real gate — a peer
//     that cannot traverse the directory never reaches the socket at all;
//   - the socket itself is chmod'ed to 0600 as defense in depth;
//   - the server additionally checks peer credentials per connection (auth.go),
//     which is what rejects root, since root ignores both of the above.
//
// The chmod in step 2 lands just after net.Listen, so the socket briefly
// carries umask-derived permissions. That window is why the 0700 directory is
// the load-bearing control rather than the socket mode.
func Listen(socketPath string) (net.Listener, error) {
	dir := filepath.Dir(socketPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("daemon: create socket dir: %w", err)
	}
	// MkdirAll respects the umask and is a no-op when the directory already
	// exists, which is the normal case (~/.devstrap predates the daemon), so the
	// explicit chmod is the part that actually enforces 0700.
	//
	//nolint:gosec // G302 does not distinguish files from directories: a directory
	// needs the execute bit to be traversable at all, so 0700 is the tightest
	// mode that can be applied here, not a loosening.
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("daemon: secure socket dir: %w", err)
	}

	if err := clearStaleSocket(socketPath); err != nil {
		return nil, err
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("daemon: listen: %w", err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("daemon: secure socket: %w", err)
	}
	// net.UnixListener unlinks the socket on Close by default
	// (SetUnlinkOnClose), so a graceful shutdown leaves no stale file behind and
	// the takeover path below is only reached after a crash or SIGKILL.
	return listener, nil
}

// clearStaleSocket removes a socket file left by a dead process, and refuses
// when the socket is still live or when the path holds something that is not a
// socket at all.
func clearStaleSocket(socketPath string) error {
	info, err := os.Lstat(socketPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("daemon: stat socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		// Never delete something we did not create. A regular file, directory,
		// or symlink here is a misconfiguration the operator must resolve.
		return fmt.Errorf("daemon: %s exists and is not a socket (mode %s); refusing to remove it", socketPath, info.Mode())
	}

	conn, err := net.DialTimeout("unix", socketPath, dialProbeTimeout)
	if err == nil {
		_ = conn.Close()
		return fmt.Errorf("%w: %s is live", ErrAlreadyRunning, socketPath)
	}
	// The dial failed, so no process is accepting on this socket: it is a
	// leftover from a crash. Remove it and let the caller bind fresh.
	if err := os.Remove(socketPath); err != nil {
		return fmt.Errorf("daemon: remove stale socket: %w", err)
	}
	return nil
}
