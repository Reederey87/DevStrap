package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
	"github.com/spf13/viper"
)

// newScanLoopEnv builds a migrated store with a workspace + local device, then
// closes it so the code under test (which opens its own store) is the sole
// writer. It returns opts pointing at the same home/root and the workspace root.
// DEVSTRAP_NO_KEYCHAIN forces file-backed key custody so event signing never
// touches a real keychain (P6-XP-04).
func newScanLoopEnv(t *testing.T) (*options, string) {
	t.Helper()
	t.Setenv("DEVSTRAP_NO_KEYCHAIN", "1")
	ctx := context.Background()
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
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
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureWorkspace(ctx, "test", root); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureDevice(ctx, "device-a"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	v := viper.New()
	v.Set("home", home)
	v.Set("root", root)
	return &options{v: v}, root
}

// initRepoWithRemote creates a real git repo with a valid origin so scan
// classifies it as a git_repo finding (not local_git).
func initRepoWithRemote(t *testing.T, path, remote string) {
	t.Helper()
	runGit(t, path, "init", "-b", "main")
	runGit(t, path, "remote", "add", "origin", remote)
}

// scanLoopCountEvents opens a fresh store and counts events of the given type.
func scanLoopCountEvents(t *testing.T, opts *options, eventType string) int {
	t.Helper()
	ctx := context.Background()
	store, err := opts.openState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	events, err := store.PendingEvents(ctx)
	if err != nil {
		t.Fatalf("PendingEvents: %v", err)
	}
	n := 0
	for _, e := range events {
		if e.Type == eventType {
			n++
		}
	}
	return n
}

// scanLoopCountProjects opens a fresh store and counts adopted projects.
func scanLoopCountProjects(t *testing.T, opts *options) int {
	t.Helper()
	ctx := context.Background()
	store, err := opts.openState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	projects, err := store.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	return len(projects)
}

// TestRunLoopScanAdoptIdempotentAndPicksUpNewRepos (P6-XP-03): the per-tick
// scan+adopt step adopts a new repo exactly once, is idempotent across repeated
// ticks over an unchanged tree (no duplicate project.added events), and still
// picks up a repo that appears between ticks without re-adopting the old one.
func TestRunLoopScanAdoptIdempotentAndPicksUpNewRepos(t *testing.T) {
	opts, root := newScanLoopEnv(t)
	ctx := context.Background()
	var stderr bytes.Buffer

	initRepoWithRemote(t, filepath.Join(root, "work", "acme", "api"), "https://github.com/acme/api.git")

	// First tick: the repo is adopted once.
	if err := runLoopScanAdopt(ctx, &stderr, opts); err != nil {
		t.Fatalf("first scan+adopt: %v", err)
	}
	if got := scanLoopCountEvents(t, opts, dssync.EventProjectAdded); got != 1 {
		t.Fatalf("project.added events after first tick = %d, want 1", got)
	}
	if got := scanLoopCountProjects(t, opts); got != 1 {
		t.Fatalf("projects after first tick = %d, want 1", got)
	}

	// Second tick over the UNCHANGED tree: idempotent — no new event.
	stderr.Reset()
	if err := runLoopScanAdopt(ctx, &stderr, opts); err != nil {
		t.Fatalf("second scan+adopt: %v", err)
	}
	if got := scanLoopCountEvents(t, opts, dssync.EventProjectAdded); got != 1 {
		t.Fatalf("project.added events after idempotent tick = %d, want 1 (no duplicate)", got)
	}
	if strings.Contains(stderr.String(), "scan adopted") {
		t.Fatalf("idempotent tick reported adoption: %q", stderr.String())
	}

	// A new repo appears between ticks: it IS adopted next tick; the existing
	// one is NOT re-adopted.
	stderr.Reset()
	initRepoWithRemote(t, filepath.Join(root, "work", "acme", "web"), "https://github.com/acme/web.git")
	if err := runLoopScanAdopt(ctx, &stderr, opts); err != nil {
		t.Fatalf("third scan+adopt: %v", err)
	}
	if got := scanLoopCountEvents(t, opts, dssync.EventProjectAdded); got != 2 {
		t.Fatalf("project.added events after new repo = %d, want 2 (one per repo)", got)
	}
	if got := scanLoopCountProjects(t, opts); got != 2 {
		t.Fatalf("projects after new repo = %d, want 2", got)
	}

	// One more tick over the now-unchanged tree stays idempotent.
	if err := runLoopScanAdopt(ctx, &stderr, opts); err != nil {
		t.Fatalf("fourth scan+adopt: %v", err)
	}
	if got := scanLoopCountEvents(t, opts, dssync.EventProjectAdded); got != 2 {
		t.Fatalf("project.added events after settling = %d, want 2", got)
	}
}

// TestRunLoopScanSkipsDuplicateRemotes (P6-XP-03 fail-safe): when two checkouts
// share a remote the loop refuses to guess a canonical path — neither is
// auto-adopted and the duplicate is surfaced on stderr.
func TestRunLoopScanSkipsDuplicateRemotes(t *testing.T) {
	opts, root := newScanLoopEnv(t)
	ctx := context.Background()

	initRepoWithRemote(t, filepath.Join(root, "work", "a"), "https://github.com/acme/dup.git")
	initRepoWithRemote(t, filepath.Join(root, "work", "b"), "https://github.com/acme/dup.git")

	var stderr bytes.Buffer
	if err := runLoopScanAdopt(ctx, &stderr, opts); err != nil {
		t.Fatalf("scan+adopt: %v", err)
	}
	if !strings.Contains(stderr.String(), "duplicate remote github.com/acme/dup") {
		t.Fatalf("stderr = %q, want duplicate-remote warning", stderr.String())
	}
	if got := scanLoopCountEvents(t, opts, dssync.EventProjectAdded); got != 0 {
		t.Fatalf("project.added events = %d, want 0 (duplicate remotes not auto-adopted)", got)
	}
	if got := scanLoopCountProjects(t, opts); got != 0 {
		t.Fatalf("projects = %d, want 0", got)
	}
}

// TestRunLoopScanWarnsSecretWithoutAdopting (P6-XP-03 fail-safe): a
// secret-looking file surfaces a stderr warning and is never adopted.
func TestRunLoopScanWarnsSecretWithoutAdopting(t *testing.T) {
	opts, root := newScanLoopEnv(t)
	ctx := context.Background()

	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("TOKEN=shh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if err := runLoopScanAdopt(ctx, &stderr, opts); err != nil {
		t.Fatalf("scan+adopt: %v", err)
	}
	if !strings.Contains(stderr.String(), "secret-looking file found: .env") {
		t.Fatalf("stderr = %q, want secret-looking-file warning", stderr.String())
	}
	if got := scanLoopCountEvents(t, opts, dssync.EventProjectAdded); got != 0 {
		t.Fatalf("project.added events = %d, want 0 (secret never adopted)", got)
	}
	if got := scanLoopCountProjects(t, opts); got != 0 {
		t.Fatalf("projects = %d, want 0", got)
	}
}
