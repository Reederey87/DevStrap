package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/config"
	"github.com/Reederey87/DevStrap/internal/daemon"
	"github.com/Reederey87/DevStrap/internal/platform"
)

func watchHealth(watchedDirs *int, err error) func(context.Context, string) (daemon.Health, error) {
	return func(context.Context, string) (daemon.Health, error) {
		return daemon.Health{Watch: daemon.WatchHealth{WatchedDirs: watchedDirs}}, err
	}
}

func TestDoctorWatchRowRendersInEveryState(t *testing.T) {
	limit := platform.InotifyLimits{MaxUserWatches: 100, MaxUserWatchesKnown: true}
	count := 34
	cases := []struct {
		name   string
		limits platform.InotifyLimits
		health func(context.Context, string) (daemon.Health, error)
		want   string
	}{
		{name: "daemon down", limits: limit, health: watchHealth(nil, daemon.ErrUnavailable), want: "daemon not running"},
		{name: "budget unknown", health: watchHealth(nil, errors.New("must not be called")), want: "inotify limit unreadable; skipped"},
		{name: "count unknown", limits: limit, health: watchHealth(nil, nil), want: "watcher degraded — see the watch plane check"},
		{name: "known", limits: limit, health: watchHealth(&count, nil), want: "34 of 100 inotify watches (34%)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkWatchBudgetWithHealth(t.Context(), config.Paths{Home: shortTestDir(t)}, tc.limits, "linux", tc.health)
			if len(got) != 1 || got[0].Name != "watch budget" || !strings.Contains(got[0].Detail, tc.want) {
				t.Fatalf("watch row = %+v, want one rendered row containing %q", got, tc.want)
			}
		})
	}
}

func TestDoctorWatchWarnsAboveThreshold(t *testing.T) {
	limit := platform.InotifyLimits{MaxUserWatches: 100, MaxUserWatchesKnown: true}
	for _, tc := range []struct {
		pct  int
		want checkStatus
	}{{59, checkOK}, {60, checkWarn}, {61, checkWarn}} {
		t.Run(fmt.Sprint(tc.pct), func(t *testing.T) {
			count := tc.pct
			got := checkWatchBudgetWithHealth(t.Context(), config.Paths{Home: shortTestDir(t)}, limit, "linux", watchHealth(&count, nil))
			if len(got) != 1 || got[0].Status != tc.want || !strings.Contains(got[0].Detail, fmt.Sprintf("(%d%%)", tc.pct)) {
				t.Fatalf("rendered watch row = %+v, want status %s and %d%%", got, tc.want, tc.pct)
			}
		})
	}
}

func TestDoctorWatchRemedyNamesSysctl(t *testing.T) {
	count := 60
	limit := platform.InotifyLimits{MaxUserWatches: 100, MaxUserWatchesKnown: true}
	got := checkWatchBudgetWithHealth(t.Context(), config.Paths{Home: shortTestDir(t)}, limit, "linux", watchHealth(&count, nil))
	if len(got) != 1 || !strings.Contains(got[0].Detail, "sudo sysctl fs.inotify.max_user_watches=400") ||
		!strings.Contains(got[0].Detail, "echo 'fs.inotify.max_user_watches=400' | sudo tee /etc/sysctl.d/99-devstrap-inotify.conf") {
		t.Fatalf("warning remedy = %+v, want current-boot and persistent sysctl forms", got)
	}
}
