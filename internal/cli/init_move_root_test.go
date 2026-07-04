package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
)

func TestInitReRunSameRootSucceeds(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("initial init stderr = %q err = %v", stderr, err)
	}
	before := readConfig(t, home)

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "init")
	if err != nil {
		t.Fatalf("re-init same root stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if after := readConfig(t, home); after != before {
		t.Fatalf("config changed on same-root re-init:\nbefore=%q\nafter=%q", before, after)
	}
}

func TestInitReRunNewRootRefusedWithConflict(t *testing.T) {
	cases := []struct {
		name string
		args func(home, newRoot string) []string
		env  bool
	}{
		{
			name: "positional",
			args: func(home, newRoot string) []string {
				return []string{"--home", home, "init", newRoot}
			},
		},
		{
			name: "root flag",
			args: func(home, newRoot string) []string {
				return []string{"--home", home, "--root", newRoot, "init"}
			},
		},
		{
			name: "env root",
			args: func(home, newRoot string) []string {
				return []string{"--home", home, "init"}
			},
			env: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), ".devstrap")
			oldRoot := filepath.Join(t.TempDir(), "OldCode")
			newRoot := filepath.Join(t.TempDir(), "NewCode")
			if _, stderr, err := executeForTest("--home", home, "--root", oldRoot, "init"); err != nil {
				t.Fatalf("initial init stderr = %q err = %v", stderr, err)
			}
			if tc.env {
				t.Setenv("DEVSTRAP_ROOT", newRoot)
			}

			_, stderr, err := executeForTest(tc.args(home, newRoot)...)
			if err == nil {
				t.Fatal("re-init with a different root succeeded, want conflict")
			}
			var app appError
			if !errors.As(err, &app) || app.code != exitConflict {
				t.Fatalf("err = %v, want appError exitConflict", err)
			}
			if !strings.Contains(stderr, oldRoot) || !strings.Contains(stderr, newRoot) || !strings.Contains(stderr, "--move-root") {
				t.Fatalf("stderr = %q, want old root %q, new root %q, and --move-root remedy", stderr, oldRoot, newRoot)
			}
			if cfg := readConfig(t, home); !strings.Contains(cfg, oldRoot) || strings.Contains(cfg, newRoot) {
				t.Fatalf("config = %q, want old root preserved after refused move", cfg)
			}
		})
	}
}

func TestInitMoveRootRewritesConfig(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	oldRoot := filepath.Join(t.TempDir(), "OldCode")
	newRoot := filepath.Join(t.TempDir(), "NewCode")
	if _, stderr, err := executeForTest("--home", home, "--root", oldRoot, "init"); err != nil {
		t.Fatalf("initial init stderr = %q err = %v", stderr, err)
	}
	// A user-added setting (and comment) must survive the move — --move-root
	// updates the root line surgically, it does not regenerate the file from
	// the default template.
	cfgPath := filepath.Join(home, "config.yaml")
	seeded, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, append(seeded, []byte("# keep me\nhub: \"r2://devstrap-test\"\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEVSTRAP_ROOT", newRoot)

	if _, stderr, err := executeForTest("--home", home, "init", "--move-root"); err != nil {
		t.Fatalf("init --move-root stderr = %q err = %v", stderr, err)
	}
	cfg := readConfig(t, home)
	if !strings.Contains(cfg, `root: "`+newRoot+`"`) || strings.Contains(cfg, oldRoot) {
		t.Fatalf("config = %q, want new root %q and no old root %q", cfg, newRoot, oldRoot)
	}
	if !strings.Contains(cfg, `hub: "r2://devstrap-test"`) || !strings.Contains(cfg, "# keep me") {
		t.Fatalf("config = %q, want user-added hub setting and comment preserved across --move-root", cfg)
	}
	info, err := os.Stat(filepath.Join(home, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %s, want 0600", info.Mode().Perm())
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".config.yaml.tmp-") {
			t.Fatalf("temporary config file left behind: %s", entry.Name())
		}
	}

	store, err := state.Open(context.Background(), filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	summary, err := store.Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary.RootPath != newRoot {
		t.Fatalf("DB root = %q, want %q", summary.RootPath, newRoot)
	}
}
