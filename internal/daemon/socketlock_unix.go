//go:build darwin || linux

package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const socketLockFile = "devstrapd.lock"

// lockSocketTakeover serializes the stale-socket probe, removal, and bind.
// flock is intentionally used instead of a created-file ownership protocol:
// the kernel releases this lock when its process exits, so SIGKILL cannot wedge
// the next daemon start. The permanent lock file must never be unlinked while
// locked, because a contender could then create and lock a different inode.
func lockSocketTakeover(dir string) (func(), error) {
	path := filepath.Join(dir, socketLockFile)
	//nolint:gosec // G304 flags any variable path; dir is the caller-selected,
	// already-created and 0700-secured state home, and the fixed basename
	// cannot escape it.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("daemon: open socket takeover lock: %w", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, fmt.Errorf("%w: socket takeover is in progress", ErrAlreadyRunning)
		}
		return nil, fmt.Errorf("daemon: lock socket takeover: %w", err)
	}
	return func() { _ = file.Close() }, nil
}
