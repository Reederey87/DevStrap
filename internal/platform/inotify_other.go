//go:build !linux

package platform

// ReadInotifyLimits reports unknown on platforms without Linux inotify. It
// deliberately ignores procRoot: a fixture that resembles procfs does not make
// inotify available on macOS or Windows.
func ReadInotifyLimits(_ string) InotifyLimits {
	return InotifyLimits{}
}
