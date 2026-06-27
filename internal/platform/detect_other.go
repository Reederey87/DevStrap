//go:build !darwin && !linux

package platform

import "runtime"

func Detect() Set {
	return newSet(
		runtime.GOOS,
		PollWatcher{},
		UnsupportedServiceManager{Platform: runtime.GOOS},
		UnsupportedKeychain{Platform: runtime.GOOS},
	)
}
