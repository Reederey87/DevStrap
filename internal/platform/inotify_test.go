package platform

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInotifyLimitsParse(t *testing.T) {
	cases := []struct {
		name    string
		content *string
		asDir   bool
		want    int
		known   bool
	}{
		{name: "valid", content: stringPtr("16384"), want: 16384, known: true},
		{name: "surrounding whitespace", content: stringPtr("  32768\n"), want: 32768, known: true},
		{name: "missing"},
		{name: "unreadable", asDir: true},
		{name: "empty", content: stringPtr("")},
		{name: "non-numeric", content: stringPtr("many")},
		{name: "negative", content: stringPtr("-1")},
		{name: "zero", content: stringPtr("0")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "sys", "fs", "inotify", "max_user_watches")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.asDir:
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			case tc.content != nil:
				if err := os.WriteFile(path, []byte(*tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got := parseInotifyLimits(root)
			if got.MaxUserWatchesKnown != tc.known || got.MaxUserWatches != tc.want {
				t.Fatalf("parseInotifyLimits() watches = (%d, %v), want (%d, %v)", got.MaxUserWatches, got.MaxUserWatchesKnown, tc.want, tc.known)
			}
			if !tc.known && got.MaxUserWatches != 0 {
				t.Fatalf("unknown limit returned silently usable value %d", got.MaxUserWatches)
			}
		})
	}
}

func TestInotifyLimitsUnknownOnMissingProcRoot(t *testing.T) {
	limits := parseInotifyLimits(filepath.Join(t.TempDir(), "does-not-exist"))
	if limits.MaxUserWatchesKnown || limits.MaxUserWatches != 0 || limits.MaxUserInstancesKnown || limits.MaxUserInstances != 0 {
		t.Fatalf("parseInotifyLimits(missing) = %+v, want all unknown", limits)
	}
}

func stringPtr(value string) *string { return &value }
