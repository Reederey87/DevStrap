//go:build linux

package platform

// ReadInotifyLimits reads Linux's per-UID inotify watch and instance budgets
// from procRoot. Individual unreadable or invalid values remain unknown rather
// than being replaced with a kernel-version-dependent guess.
func ReadInotifyLimits(procRoot string) InotifyLimits {
	return parseInotifyLimits(procRoot)
}
