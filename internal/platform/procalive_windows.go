//go:build windows

package platform

import (
	"errors"
	"math"

	"golang.org/x/sys/windows"
)

// ProcessAlive reports whether pid is alive. Access-denied (a live process
// owned by another user) is treated as alive — see procalive_unix.go for the
// shared fail-safe rationale, which this mirrors.
//
// Liveness is determined via WaitForSingleObject on a SYNCHRONIZE handle, not
// GetExitCodeProcess's STILL_ACTIVE (259) sentinel: Microsoft's own docs note
// GetExitCodeProcess cannot distinguish a running process from one that
// legitimately exited with code 259, so relying on it is unreliable
// (CodeRabbit review, PR #198, citing
// https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-getexitcodeprocess).
// A process handle is not an auto-reset synchronization object — it stays
// permanently signaled once the process exits — so polling it repeatedly
// with a zero timeout is safe and does not consume/alter its state.
// SYNCHRONIZE alone (not combined with PROCESS_QUERY_LIMITED_INFORMATION) is
// requested to minimize the chance OpenProcess denies access, since a
// combined access mask can only succeed if every requested right is granted.
func ProcessAlive(pid int) bool {
	// Widen to uint64 before comparing: on windows/386 (32-bit int), `pid >
	// math.MaxUint32` would overflow the untyped constant into int and fail
	// to compile. DevStrap doesn't ship a 386 build today, but this file must
	// still compile for any windows/GOARCH the module supports.
	if pid <= 0 || uint64(pid) > math.MaxUint32 {
		return false
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
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

	event, err := windows.WaitForSingleObject(handle, 0)
	if err != nil {
		return true // Couldn't determine signaled state; indeterminate, not provably dead.
	}
	switch event {
	case windows.WAIT_OBJECT_0:
		return false // The process object is signaled: the process has terminated.
	case uint32(windows.WAIT_TIMEOUT): // WAIT_TIMEOUT is a syscall.Errno constant, not a uint32.
		return true // Not yet signaled within the zero timeout: still running.
	default:
		return true // WAIT_ABANDONED/WAIT_FAILED or anything else: indeterminate, not provably dead.
	}
}
