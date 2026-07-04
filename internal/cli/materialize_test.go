package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/viper"
)

func TestQuietSuppressesMaterializeHumanProgressOnly(t *testing.T) {
	cases := []struct {
		name             string
		args             []string
		wantHumanSummary bool
		wantJSON         bool
	}{
		{name: "plain", wantHumanSummary: true},
		{name: "quiet", args: []string{"--quiet"}},
		{name: "quiet json", args: []string{"--quiet", "--json"}, wantJSON: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home, root, projectDir := setupPlainFolderSkeleton(t)
			args := append([]string{"--home", home, "--root", root}, tc.args...)
			args = append(args, "materialize")
			stdout, stderr, err := executeForTest(args...)
			if err != nil {
				t.Fatalf("stdout = %q stderr = %q err = %v", stdout, stderr, err)
			}
			hasSummary := strings.Contains(stdout, "Materialized")
			if hasSummary != tc.wantHumanSummary {
				t.Fatalf("stdout = %q, contains materialize summary = %v, want %v", stdout, hasSummary, tc.wantHumanSummary)
			}
			if tc.wantJSON {
				var got struct {
					Total     int `json:"total"`
					Succeeded int `json:"succeeded"`
					Skipped   int `json:"skipped"`
					Failed    int `json:"failed"`
				}
				if err := json.Unmarshal([]byte(stdout), &got); err != nil {
					t.Fatalf("materialize --json stdout is not valid JSON: %v\n%s", err, stdout)
				}
				if got.Total != 1 || got.Succeeded != 1 || got.Skipped != 0 || got.Failed != 0 {
					t.Fatalf("json result = %+v, want one successful materialization", got)
				}
			}
			if info, err := os.Stat(projectDir); err != nil || !info.IsDir() {
				t.Fatalf("materialize side effect missing %s: info=%v err=%v", projectDir, info, err)
			}
		})
	}
}

func setupPlainFolderSkeleton(t *testing.T) (home, root, projectDir string) {
	t.Helper()
	ctx := context.Background()
	home = filepath.Join(t.TempDir(), ".devstrap")
	root = filepath.Join(t.TempDir(), "Code")
	projectPath := "work/acme/plain"
	projectDir = filepath.Join(root, filepath.FromSlash(projectPath))
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureWorkspace(ctx, "test", root); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureDevice(ctx, "device-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertProject(ctx, state.UpsertProjectParams{
		Path:                 projectPath,
		Type:                 "plain_folder",
		MaterializationState: "skeleton",
		DirtyState:           "clean",
	}); err != nil {
		t.Fatal(err)
	}
	return home, root, projectDir
}

func TestMaterializeRebuildsBeforeHydrate(t *testing.T) {
	ctx := context.Background()
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	repoPath := filepath.Join(root, "work", "acme", "api")
	runGit(t, repoPath, "init")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}

	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureWorkspace(ctx, "test", root); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureDevice(ctx, "device-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertProject(ctx, state.UpsertProjectParams{
		Path:                 "work/acme/api",
		Type:                 "git_repo",
		RemoteURL:            "https://github.com/acme/api.git",
		RemoteKey:            "github.com/acme/api",
		LocalPath:            repoPath,
		MaterializationState: "available",
		DirtyState:           "clean",
	}); err != nil {
		t.Fatal(err)
	}
	project, err := store.ProjectByPath(ctx, "work/acme/api")
	if err != nil {
		t.Fatal(err)
	}
	v := viper.New()
	v.Set("home", home)
	v.Set("root", root)
	opts := &options{v: v}

	var calls []string
	oldRebuild := materializeRebuildDependencies
	oldHydrate := materializeHydrateProjectEnv
	t.Cleanup(func() {
		materializeRebuildDependencies = oldRebuild
		materializeHydrateProjectEnv = oldHydrate
	})
	materializeRebuildDependencies = func(_ context.Context, _, _, _ string) error {
		calls = append(calls, "rebuild")
		return nil
	}
	materializeHydrateProjectEnv = func(_ context.Context, _ *state.Store, _ *options, _ state.ProjectStatus, _ string) error {
		calls = append(calls, "hydrate")
		return nil
	}
	t.Setenv("DEVSTRAP_REBUILD_DEPS", "1")

	if err := materializeGitRepo(ctx, store, opts, project, true); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(calls, ","), "rebuild,hydrate"; got != want {
		t.Fatalf("call order = %q, want %q", got, want)
	}
}

func TestMaterializeRebuildLogIsWritten0600(t *testing.T) {
	ctx := context.Background()
	home := filepath.Join(t.TempDir(), ".devstrap")
	localPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(localPath, "go.mod"), []byte("module example.com/rebuild\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fakeBin := t.TempDir()
	fakeGo := filepath.Join(fakeBin, "go")
	script := `#!/bin/sh
echo "stdout from rebuild"
echo "stderr from rebuild" >&2
`
	if err := os.WriteFile(fakeGo, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := rebuildDependencies(ctx, home, "work/acme/api-server", localPath); err != nil {
		t.Fatal(err)
	}
	logPath := rebuildLogPath(home, "work/acme/api-server")
	if !strings.Contains(filepath.Base(logPath), "work_acme_api-server-") {
		t.Fatalf("log path %q lost its readable sanitized prefix", logPath)
	}
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("log mode = %s, want 0600", info.Mode().Perm())
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	if !strings.Contains(content, "stdout from rebuild") || !strings.Contains(content, "stderr from rebuild") {
		t.Fatalf("log content = %q, want stdout and stderr", content)
	}
}
