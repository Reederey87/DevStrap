//go:build !linux && !darwin && !windows

package platform

// ProcessAlive reports a fail-safe liveness result on unsupported platforms.
// Liveness cannot be determined here, so it never wrongly claims a lock is
// stealable.
func ProcessAlive(int) bool {
	return true
}
