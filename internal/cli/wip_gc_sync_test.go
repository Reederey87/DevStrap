package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/viper"
)

func TestWipGCDurationConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
		raw  string
		want time.Duration
	}{
		{"interval empty uses default", wipGCIntervalKey, "", defaultWipGCInterval},
		{"interval zero disables", wipGCIntervalKey, "0", 0},
		{"ttl empty uses default", wipTTLConfigKey, "", defaultWipGCTTL},
		{"ttl zero disables", wipTTLConfigKey, "0", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := viper.New()
			v.Set(tc.key, tc.raw)
			opts := &options{v: v}
			var got time.Duration
			var err error
			if tc.key == wipGCIntervalKey {
				got, err = wipGCInterval(opts)
			} else {
				got, err = wipTTL(opts)
			}
			if err != nil || got != tc.want {
				t.Fatalf("duration = %s, err %v; want %s", got, err, tc.want)
			}
		})
	}
	for _, raw := range []string{"-1s", "tomorrow"} {
		t.Run("invalid "+raw, func(t *testing.T) {
			v := viper.New()
			v.Set(wipGCIntervalKey, raw)
			_, err := wipGCInterval(&options{v: v})
			var appErr appError
			if !errors.As(err, &appErr) || appErr.code != exitInvalidConfig {
				t.Fatalf("wipGCInterval(%q) = %v, want exitInvalidConfig", raw, err)
			}
			v.Set(wipTTLConfigKey, raw)
			_, err = wipTTL(&options{v: v})
			if !errors.As(err, &appErr) || appErr.code != exitInvalidConfig {
				t.Fatalf("wipTTL(%q) = %v, want exitInvalidConfig", raw, err)
			}
		})
	}
}

func TestWipGCDueTreatsCorruptRecordAsDueNow(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home := filepath.Join(t.TempDir(), "home")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init: %v (%s)", err, stderr)
	}
	store := openTestStore(t, home)
	defer closeStore(store)
	if err := store.SetLocalMeta(t.Context(), wipGCLastSuccessKey, "{broken"); err != nil {
		t.Fatal(err)
	}
	due, err := wipGCDue(t.Context(), store, time.Hour, time.Now())
	if err != nil || !due {
		t.Fatalf("corrupt record due = %v, err %v; want true, nil", due, err)
	}
}

func TestCheckWipGCFreshness(t *testing.T) {
	opts, store, _, _ := setupMaterializedWipGCProject(t)
	defer closeStore(store)
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	opts.v.Set(wipGCIntervalKey, "1h")
	opts.v.Set(wipTTLConfigKey, "720h")
	raw := `"2026-07-30T09:00:00Z"`
	if err := store.SetLocalMeta(t.Context(), wipGCLastSuccessKey, raw); err != nil {
		t.Fatal(err)
	}
	got := checkWipGCFreshness(t.Context(), opts, store, now)
	if len(got) != 1 || got[0].Status != checkWarn || !strings.Contains(got[0].Detail, "3h0m0s") {
		t.Fatalf("stale freshness = %+v, want warning past 2x interval", got)
	}
	opts.v.Set(wipTTLConfigKey, "0")
	got = checkWipGCFreshness(t.Context(), opts, store, now)
	if len(got) != 1 || got[0].Status != checkOK || !strings.Contains(got[0].Detail, "disabled") {
		t.Fatalf("disabled freshness = %+v, want disabled OK", got)
	}
}

func TestWipGCLongIntervalSkipsSecondEnumerationAndKeepsTimestamp(t *testing.T) {
	opts, store, repo, remote := setupMaterializedWipGCProject(t)
	defer closeStore(store)
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	opts.v.Set(wipGCIntervalKey, "24h")
	opts.v.Set(wipTTLConfigKey, "720h")
	if _, err := maybeGCWipRefsAfterSync(t.Context(), io.Discard, opts, store, now); err != nil {
		t.Fatal(err)
	}
	first, ok, err := store.GetLocalMeta(t.Context(), wipGCLastSuccessKey)
	if err != nil || !ok {
		t.Fatalf("first marker = %q ok %v err %v", first, ok, err)
	}
	moved := remote + ".offline"
	if err := os.Rename(remote, moved); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if _, err := maybeGCWipRefsAfterSync(t.Context(), &stderr, opts, store, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	second, ok, err := store.GetLocalMeta(t.Context(), wipGCLastSuccessKey)
	if err != nil || !ok || second != first {
		t.Fatalf("second marker = %q ok %v err %v; want unchanged %q", second, ok, err, first)
	}
	if stderr.Len() != 0 {
		t.Fatalf("second pass touched moved origin %s from repo %s: %s", moved, repo, stderr.String())
	}
}

func TestRunSyncCycleIsolatesUnavailableWipGCOrigin(t *testing.T) {
	opts, store, _, remote := setupMaterializedWipGCProject(t)
	closeStore(store)
	opts.v.Set(wipGCIntervalKey, "1ms")
	opts.v.Set(wipTTLConfigKey, "1ms")
	hubPath := filepath.Join(t.TempDir(), "hub.json")
	if err := runSyncCycle(t.Context(), io.Discard, io.Discard, opts, hubPath, false, false); err != nil {
		t.Fatalf("priming sync: %v", err)
	}
	moved := remote + ".offline"
	if err := os.Rename(remote, moved); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	var stderr bytes.Buffer
	if err := runSyncCycle(t.Context(), io.Discard, &stderr, opts, hubPath, false, false); err != nil {
		t.Fatalf("sync with unavailable WIP origin must exit 0: %v\n%s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "automatic WIP GC") {
		t.Fatalf("stderr = %q, want recorded automatic WIP GC warning", stderr.String())
	}
	store = openTestStore(t, opts.paths().Home)
	defer closeStore(store)
	project, err := store.ProjectByPath(t.Context(), "work/proj")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(project.LastError, "wip gc: ") {
		t.Fatalf("project warning = %q, want wip gc prefix", project.LastError)
	}
}

func setupMaterializedWipGCProject(t *testing.T) (*options, *state.Store, string, string) {
	t.Helper()
	t.Setenv(platform.NoKeychainEnv, "1")
	base := t.TempDir()
	home := filepath.Join(base, "home")
	root := filepath.Join(base, "Code")
	remote := filepath.Join(base, "origin.git")
	runGit(t, base, "init", "--bare", "-b", "main", remote)
	repo := filepath.Join(root, "work", "proj")
	runGit(t, root, "clone", remote, repo)
	runGit(t, repo, "config", "user.name", "DevStrap Test")
	runGit(t, repo, "config", "user.email", "test@example.invalid")
	runGit(t, repo, "commit", "--allow-empty", "-m", "init")
	runGit(t, repo, "push", "origin", "main")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init: %v (%s)", err, stderr)
	}
	opts := testOptions(home, root)
	opts.v = viper.New()
	opts.v.Set("home", home)
	opts.v.Set("root", root)
	store, err := opts.openState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertProject(t.Context(), state.UpsertProjectParams{
		Path:                 "work/proj",
		Type:                 "git_repo",
		RemoteURL:            remote,
		RemoteKey:            remote,
		LocalPath:            repo,
		MaterializationState: "available",
		DirtyState:           "clean",
		DefaultBranch:        "main",
	}); err != nil {
		closeStore(store)
		t.Fatal(err)
	}
	return opts, store, repo, remote
}
