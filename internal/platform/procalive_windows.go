//go:build windows

package platform

import (
	"errors"

	"golang.org/x/sys/windows"
)

// ProcessAlive reports whether pid is alive. Access-denied (a live process
// owned by another user) is treated as alive — see procalive_unix.go for the
// shared fail-safe rationale, which this mirrors.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		switch {
		case errors.Is(err, windows.ERROR_ACCESS_DENIED):
			return true
		case errors.Is(err, windows.ERROR_INVALID_PARAMETER):
			return false // OpenProcess uses this for a PID that does not exist.
		default:
			return true // Indeterminate, not provably dead.
		}
	}
	defer func() { _ = windows.CloseHandle(handle) }()

	var exitCode uint32
	if err := windows.GetExitCodeProcess(handle, &exitCode); err != nil {
		return true // Couldn't read the exit code; indeterminate, not provably dead.
	}
	const stillActive = 259 // STILL_ACTIVE is not exported by x/sys/windows.
	return exitCode == stillActive
}
