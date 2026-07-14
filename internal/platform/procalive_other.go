//go:build !linux && !darwin && !windows

package platform

// ProcessAlive reports a fail-safe liveness result on unsupported platforms.
// Liveness cannot be determined here, so it never wrongly claims a lock is
// stealable — except for a non-positive pid, which every build-tagged
// variant treats as trivially dead (matching procalive_unix.go/
// procalive_windows.go's pid<=0 guard; see procalive_test.go, which keeps
// its nonexistent-but-positive-PID assertion out of this build's test set
// since this fallback cannot determine that case).
func ProcessAlive(pid int) bool {
	return pid > 0
}
