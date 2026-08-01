package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/state"
)

func stagingSweepFixture(t *testing.T) (*options, *state.Store, string) {
	t.Helper()
	t.Setenv(platform.NoKeychainEnv, "1")
	home := filepath.Join(t.TempDir(), "home")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init: %v (%s)", err, stderr)
	}
	opts := testOptions(home, root)
	store, err := opts.openState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeStore(store) })
	return opts, store, root
}

func TestStagingProjectNameRightAnchored(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{{".my.app.devstrap-tmp-123", "my.app", true}, {".my.app.devstrap-tmp-alpha-Z", "my.app", true}, {"my.app.devstrap-tmp-123", "", false}, {".my.app.devstrap-tmp-", "", false}} {
		got, ok := stagingProjectName(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("stagingProjectName(%q)=(%q,%v), want (%q,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestStagingSweepDoesNotReapLiveCloneHoldingRepoLock(t *testing.T) {
	opts, store, root := stagingSweepFixture(t)
	projectPath := filepath.Join(root, "work", "acme", "my.app")
	if err := os.MkdirAll(projectPath, 0750); err != nil {
		t.Fatal(err)
	}
	project, err := store.UpsertProject(t.Context(), state.UpsertProjectParams{Path: "work/acme/my.app", Type: "git_repo", LocalPath: projectPath})
	if err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(filepath.Dir(projectPath), ".my.app.devstrap-tmp-alpha")
	if err := os.MkdirAll(filepath.Join(candidate, ".git"), 0750); err != nil {
		t.Fatal(err)
	}
	unlock, err := acquireRepoLock(opts.paths().Home, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	actions, err := sweepStagingOrphans(t.Context(), store, opts, time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(candidate); err != nil {
		t.Fatalf("live clone was reaped: %v", err)
	}
	if len(actions) != 1 || !strings.Contains(actions[0].Reason, "repo lock") {
		t.Fatalf("actions=%+v, want repo-lock skip reason", actions)
	}
}

func TestStagingSweepRefusesSymlink(t *testing.T) {
	opts, store, root := stagingSweepFixture(t)
	outside := t.TempDir()
	target := filepath.Join(outside, "keep")
	if err := os.Mkdir(target, 0750); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(root, ".x.devstrap-tmp-alpha")
	if err := os.Symlink(target, candidate); err != nil {
		t.Fatal(err)
	}
	actions, err := sweepStagingOrphans(t.Context(), store, opts, time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("symlink target changed: %v", err)
	}
	if len(actions) != 1 || !strings.Contains(actions[0].Reason, "not a real directory") {
		t.Fatalf("actions=%+v", actions)
	}
}

// TestStagingSweepDoesNotDescendIntoGeneratedDirs pins the walk's prune. Without
// it the sweep walks every node_modules/.git/.venv in the managed tree on each
// interval, and — worse than slow — it can match a directory that merely LOOKS
// like clone staging inside a dependency and delete it. A real staging dir is
// always a SIBLING of a project target, so nothing reachable only through a
// pruned directory is ever a legitimate candidate.
func TestStagingSweepDoesNotDescendIntoGeneratedDirs(t *testing.T) {
	opts, store, root := stagingSweepFixture(t)

	// A staging-shaped directory buried inside node_modules. It is old enough
	// and unmapped, so ONLY the prune keeps it alive.
	buried := filepath.Join(root, "work", "acme", "node_modules", "pkg", ".vendored.devstrap-tmp-999")
	if err := os.MkdirAll(buried, 0o750); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(buried, old, old); err != nil {
		t.Fatal(err)
	}

	actions, err := sweepStagingOrphans(t.Context(), store, opts, time.Now())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	for _, a := range actions {
		if strings.Contains(a.Path, "node_modules") {
			t.Fatalf("sweep reached inside node_modules: %+v", a)
		}
	}
	if _, err := os.Stat(buried); err != nil {
		t.Fatalf("directory inside node_modules was removed by the sweep: %v", err)
	}
}

// TestRegisteredUnderSparesAncestorOfRegisteredProject pins the review's MAJOR:
// the registered-row guard was exact-match on the candidate itself, so a project
// registered UNDERNEATH a staging-shaped ancestor would be deleted wholesale
// along with that directory.
//
// It tests the predicate directly rather than end to end, and the reason is
// itself the good news: `pathkey.Clean` now rejects a staging-shaped component
// at the STORE layer, so this row shape can no longer be created through any
// API. Only a legacy row predating that guard can be in this state — which is
// exactly what a guard is for, and exactly what an end-to-end fixture can no
// longer construct.
func TestRegisteredUnderSparesAncestorOfRegisteredProject(t *testing.T) {
	sep := string(os.PathSeparator)
	ancestor := filepath.Join("/Code", "work", ".x.devstrap-tmp-legacy")
	nested := filepath.Join(ancestor, "app")

	if under, ok := registeredUnder(ancestor, []string{nested}); !ok || under != nested {
		t.Fatalf("ancestor of a registered project was not spared: under=%q ok=%v", under, ok)
	}
	// A sibling that merely shares a textual prefix must NOT be spared, or the
	// guard would refuse to sweep real orphans.
	sibling := filepath.Join("/Code", "work", ".x.devstrap-tmp-legacy-other", "app")
	if under, ok := registeredUnder(ancestor, []string{sibling}); ok {
		t.Fatalf("prefix-only sibling wrongly spared the candidate: %q", under)
	}
	// The candidate itself being registered is the exact-match branch's job.
	if _, ok := registeredUnder(ancestor, []string{ancestor}); ok {
		t.Fatal("registeredUnder must require a STRICT descendant")
	}
	_ = sep
}

// TestStagingSweepAgeGuardBothDirections pins the ONLY protection an unmapped
// candidate has. Without both halves, setting stagingOrphanMinAge = 0 or
// deleting the branch outright passes the rest of the suite: the txtar's removed
// candidate is MAPPED, so it never reaches the mtime branch at all.
func TestStagingSweepAgeGuardBothDirections(t *testing.T) {
	for _, tc := range []struct {
		name    string
		age     time.Duration
		removed bool
	}{
		{"fresh unmapped candidate survives", 5 * time.Minute, false},
		{"aged unmapped candidate is removed", 48 * time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, store, root := stagingSweepFixture(t)
			// Unmapped: no project named "ghost" is registered.
			cand := filepath.Join(root, "work", ".ghost.devstrap-tmp-1")
			if err := os.MkdirAll(cand, 0o750); err != nil {
				t.Fatal(err)
			}
			stamp := time.Now().Add(-tc.age)
			if err := os.Chtimes(cand, stamp, stamp); err != nil {
				t.Fatal(err)
			}
			if _, err := sweepStagingOrphans(t.Context(), store, opts, time.Now()); err != nil {
				t.Fatalf("sweep: %v", err)
			}
			_, statErr := os.Stat(cand)
			gone := os.IsNotExist(statErr)
			if gone != tc.removed {
				t.Fatalf("age=%s removed=%v, want removed=%v", tc.age, gone, tc.removed)
			}
		})
	}
}
