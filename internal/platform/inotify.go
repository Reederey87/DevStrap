package platform

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultProcRoot is the procfs mount used by production Linux systems.
const DefaultProcRoot = "/proc"

// InotifyLimits reports the per-UID inotify budgets exposed by Linux. A false
// Known value means the corresponding procfs value was absent or invalid; zero
// is never substituted with a guessed kernel default.
type InotifyLimits struct {
	MaxUserWatches        int
	MaxUserWatchesKnown   bool
	MaxUserInstances      int
	MaxUserInstancesKnown bool
}

// parseInotifyLimits reads inotify limits below procRoot. Keeping procRoot an
// argument makes the parser deterministic in tests and supports nonstandard
// procfs mount points without baking /proc into the file-reading logic.
func parseInotifyLimits(procRoot string) InotifyLimits {
	watches, watchesKnown := readPositiveInt(filepath.Join(procRoot, "sys", "fs", "inotify", "max_user_watches"))
	instances, instancesKnown := readPositiveInt(filepath.Join(procRoot, "sys", "fs", "inotify", "max_user_instances"))
	return InotifyLimits{
		MaxUserWatches:        watches,
		MaxUserWatchesKnown:   watchesKnown,
		MaxUserInstances:      instances,
		MaxUserInstancesKnown: instancesKnown,
	}
}

func readPositiveInt(path string) (int, bool) {
	//nolint:gosec // path is procRoot joined with constant sysctl segments, never
	// a caller-supplied filename. procRoot is a test seam (a fixture directory)
	// or the DefaultProcRoot constant, and the result is only ever parsed as an
	// integer — no file CONTENT reaches the caller, so a redirected read cannot
	// leak anything; it degrades to "unknown", which is this reader's safe state.
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || value <= 0 {
		return 0, false
	}
	return value, true
}
