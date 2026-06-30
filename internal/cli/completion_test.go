package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
)

// P5-DX-01: `__complete open <prefix>` suggests namespace paths from the store.
func TestPathCompletionSuggestsNamespacePaths(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--workspace-name", "personal"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	ctx := context.Background()
	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertProject(ctx, state.UpsertProjectParams{Path: "work/acme/api", Type: "git_repo", RemoteKey: "github.com/acme/api", RemoteURL: "https://github.com/acme/api"}); err != nil {
		t.Fatal(err)
	}
	closeStore(store)

	// cobra's hidden __complete command drives ValidArgsFunction.
	stdout, _, err := executeForTest("--home", home, "--root", root, "__complete", "open", "work/")
	if err != nil {
		t.Fatalf("__complete err = %v", err)
	}
	if !strings.Contains(stdout, "work/acme/api") {
		t.Fatalf("__complete open stdout = %q, want it to suggest work/acme/api", stdout)
	}
}

// P5-DX-01: enum flags complete from their fixed value set.
func TestEnumFlagCompletion(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	stdout, _, err := executeForTest("--home", home, "--root", root, "__complete", "add", "--lfs-policy", "")
	if err != nil {
		t.Fatalf("__complete err = %v", err)
	}
	for _, want := range []string{"auto", "never", "agent", "always"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("__complete --lfs-policy stdout = %q, want it to include %q", stdout, want)
		}
	}
}
