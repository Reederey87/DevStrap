//go:build darwin

package platform

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// ProcessStartTime returns the process's start-time identity in raw
// microseconds. The value is opaque and is only suitable for equality
// comparisons on the same host and boot.
func ProcessStartTime(pid int) (int64, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, fmt.Errorf("read process info: %w", err)
	}
	return kp.Proc.P_starttime.Sec*1_000_000 + int64(kp.Proc.P_starttime.Usec), nil
}
