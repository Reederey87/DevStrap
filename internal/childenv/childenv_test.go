package childenv

import (
	"slices"
	"strings"
	"testing"
)

func TestBuildAllowsOnlyExplicitNamesAndStripsDangerous(t *testing.T) {
	got, err := Build(Options{
		Base: []string{
			"PATH=/usr/bin",
			"HOME=/tmp/home",
			"DEVSTRAP_TOKEN=secret",
			"LD_PRELOAD=/tmp/hook.dylib",
			"DYLD_INSERT_LIBRARIES=/tmp/hook.dylib",
			"GIT_SSH_COMMAND=ssh -oProxyCommand=evil",
			"PYTHONPATH=/tmp/python",
			"BAD-ENTRY",
		},
		Allow: []string{"PATH", "HOME", "LD_PRELOAD", "DYLD_INSERT_LIBRARIES", "GIT_SSH_COMMAND", "PYTHONPATH"},
		Set: map[string]string{
			"GIT_TERMINAL_PROMPT": "0",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantPresent := []string{"PATH=/usr/bin", "HOME=/tmp/home", "GIT_TERMINAL_PROMPT=0"}
	for _, want := range wantPresent {
		if !slices.Contains(got, want) {
			t.Fatalf("Build() = %#v, want %s", got, want)
		}
	}
	for _, pair := range got {
		if strings.Contains(pair, "secret") ||
			strings.HasPrefix(pair, "LD_PRELOAD=") ||
			strings.HasPrefix(pair, "DYLD_INSERT_LIBRARIES=") ||
			strings.HasPrefix(pair, "GIT_SSH_COMMAND=") ||
			strings.HasPrefix(pair, "PYTHONPATH=") {
			t.Fatalf("Build() leaked blocked variable: %#v", got)
		}
	}
}

func TestBuildRejectsDangerousExplicitSet(t *testing.T) {
	if _, err := Build(Options{Set: map[string]string{"GIT_SSH_COMMAND": "ssh -oProxyCommand=evil"}}); err == nil {
		t.Fatal("Build() accepted dangerous explicit set")
	}
}

func TestBuildSupportsPrefixAllowPatterns(t *testing.T) {
	got, err := Build(Options{
		Base:  []string{"XDG_CONFIG_HOME=/tmp/config", "XDG_CACHE_HOME=/tmp/cache", "XDG_BAD=/tmp/bad", "TOKEN=secret"},
		Allow: []string{"XDG_*"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got, "XDG_CONFIG_HOME=/tmp/config") || !slices.Contains(got, "XDG_CACHE_HOME=/tmp/cache") || !slices.Contains(got, "XDG_BAD=/tmp/bad") {
		t.Fatalf("Build() = %#v, want XDG_* vars", got)
	}
	if slices.Contains(got, "TOKEN=secret") {
		t.Fatalf("Build() leaked non-allowlisted token: %#v", got)
	}
}
