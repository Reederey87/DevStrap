//go:build darwin || linux

package platform

import (
	"errors"
	"os"
	"syscall"
)

// ProcessAlive reports whether pid is alive. A permission-denied or otherwise
// ambiguous result is treated as alive (fail-safe: never let a live-but-
// inaccessible process's lock be wrongly stolen) — only an explicit "no such
// process" is treated as dead.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return true // Lookup itself failed; indeterminate, not provably dead.
	}
	signalErr := process.Signal(syscall.Signal(0))
	return !errors.Is(signalErr, syscall.ESRCH) && !errors.Is(signalErr, os.ErrProcessDone)
}
