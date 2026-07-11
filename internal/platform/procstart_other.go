//go:build !linux && !darwin

package platform

import "fmt"

func ProcessStartTime(int) (int64, error) {
	return 0, fmt.Errorf("%w: process start-time identity is not implemented on this platform", ErrUnsupported)
}
