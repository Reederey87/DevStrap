package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/viper"
)

func TestMaterializeLFSAutoRecordsAvailableWithPointers(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	store, opts, project := setupHydrateLFSProject(t, home, root, "auto", true)
	defer closeStore(store)

	if err := materializeGitRepo(t.Context(), store, opts, project, true); err != nil {
		t.Fatal(err)
	}
	assertProjectMaterialization(t, store, project.Path, "available")
}

func TestMaterializeLFSAlwaysRecordsFailedWhenPullFails(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	store, opts, project := setupHydrateLFSProject(t, home, root, "always", true)
	defer closeStore(store)
	installFailingGitLFS(t)

	if err := materializeGitRepo(t.Context(), store, opts, project, true); err == nil {
		t.Fatal("expected LFS materialization failure")
	}
	assertProjectMaterialization(t, store, project.Path, "failed")
}

// TestMaterializeLFSAlwaysDoesNotFlipFailedToAvailableOnRetry pins the P6-GIT-04
// review's blocking finding: an always-policy repo recorded "failed" because its
// LFS pull failed is re-queued via SkeletonProjects (which includes "failed") on
// the next sync/run-loop tick and re-materialized. On retry hydrate early-returns
// for the already-on-disk repo, but materializeGitRepo re-applies the LFS policy
// afterward, so it must NOT silently flip to available/clean with pointer files
// still present.
func TestMaterializeLFSAlwaysDoesNotFlipFailedToAvailableOnRetry(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	store, opts, project := setupHydrateLFSProject(t, home, root, "always", true)
	defer closeStore(store)
	installFailingGitLFS(t)

	if err := materializeGitRepo(t.Context(), store, opts, project, true); err == nil {
		t.Fatal("expected first LFS materialization to fail")
	}
	assertProjectMaterialization(t, store, project.Path, "failed")

	// Retry with the repo already on disk. hydrate early-returns, then
	// materializeGitRepo re-applies the always-policy LFS pull, which still
	// fails, so it must stay "failed" — never a silent flip to available/clean.
	project2, err := store.ProjectByPath(t.Context(), project.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := materializeGitRepo(t.Context(), store, opts, project2, true); err == nil {
		t.Fatal("expected retry on the already-present repo to fail (no silent flip to available)")
	}
	assertProjectMaterialization(t, store, project.Path, "failed")
}

func TestMaterializeNoLFSUnaffected(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	store, opts, project := setupHydrateLFSProject(t, home, root, "auto", false)
	defer closeStore(store)

	if err := materializeGitRepo(t.Context(), store, opts, project, true); err != nil {
		t.Fatal(err)
	}
	assertProjectMaterialization(t, store, project.Path, "available")
}

func setupHydrateLFSProject(t *testing.T, home, root, lfsPolicy string, usesLFS bool) (*state.Store, *options, state.ProjectStatus) {
	t.Helper()
	t.Setenv("DEVSTRAP_NO_KEYCHAIN", "1")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}

	remote := createHydrateRemote(t, usesLFS)
	if _, stderr, err := executeForTest("--home", home, "--root", root, "add", "file://"+remote, "--path", "work/acme/repo", "--default-branch", "main", "--lfs-policy", lfsPolicy); err != nil {
		t.Fatalf("add stderr = %q err = %v", stderr, err)
	}

	opts := testOptions(home, root)
	store, err := opts.openState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.ProjectByPath(context.Background(), "work/acme/repo")
	if err != nil {
		closeStore(store)
		t.Fatal(err)
	}
	return store, opts, project
}

func createHydrateRemote(t *testing.T, usesLFS bool) string {
	t.Helper()
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "repo.git")
	seed := filepath.Join(tmp, "seed")
	runGit(t, tmp, "init", "--bare", remote)
	runGit(t, seed, "init")
	runGit(t, seed, "config", "user.email", "devstrap@example.test")
	runGit(t, seed, "config", "user.name", "DevStrap Test")
	runGit(t, seed, "checkout", "-b", "main")
	files := []string{"README.md"}
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if usesLFS {
		if err := os.WriteFile(filepath.Join(seed, ".gitattributes"), []byte("*.bin filter=lfs -text\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(seed, "data.bin"), []byte("regular data\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		files = append(files, ".gitattributes", "data.bin")
	}
	runGit(t, seed, append([]string{"add"}, files...)...)
	runGit(t, seed, "commit", "-m", "initial")
	runGit(t, seed, "remote", "add", "origin", remote)
	runGit(t, seed, "push", "origin", "main")
	runGit(t, tmp, "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/main")
	return remote
}

func testOptions(home, root string) *options {
	v := viper.New()
	v.Set("home", home)
	v.Set("root", root)
	v.Set("materialization.clone_timeout", "30m")
	return &options{v: v}
}

func assertProjectMaterialization(t *testing.T, store *state.Store, nsPath, want string) {
	t.Helper()
	project, err := store.ProjectByPath(context.Background(), nsPath)
	if err != nil {
		t.Fatal(err)
	}
	if project.MaterializationState != want {
		t.Fatalf("MaterializationState = %q, want %q", project.MaterializationState, want)
	}
}
