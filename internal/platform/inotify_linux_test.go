//go:build linux

package platform

import "testing"

func TestInotifyLimitsRealLinuxReader(t *testing.T) {
	limits := ReadInotifyLimits(DefaultProcRoot)
	if !limits.MaxUserWatchesKnown || limits.MaxUserWatches <= 0 {
		t.Fatalf("ReadInotifyLimits(%q) = %+v, want positive max_user_watches", DefaultProcRoot, limits)
	}
}
