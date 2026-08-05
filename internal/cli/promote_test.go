package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
)

// promoteEnv initializes a workspace and returns its home/root pair.
func promoteEnv(t *testing.T) (home, root string) {
	t.Helper()
	home = filepath.Join(t.TempDir(), ".devstrap")
	root = filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	return home, root
}

// seedPromoteProject writes a namespace row of an arbitrary type. plain_folder
// is seeded directly because `scan` does not emit it (spec/07) — the type is
// reachable in the fleet only through sync, so the lattice table has to build
// it here to cover the transition at all.
func seedPromoteProject(t *testing.T, home, root, nsPath, typ, remoteURL, remoteKey string) string {
	t.Helper()
	local := filepath.Join(root, filepath.FromSlash(nsPath))
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(t.Context(), filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	if _, err := store.UpsertProject(t.Context(), state.UpsertProjectParams{
		Path:                 nsPath,
		Type:                 typ,
		RemoteURL:            remoteURL,
		RemoteKey:            remoteKey,
		LocalPath:            local,
		MaterializationState: "available",
		DirtyState:           "unknown",
	}); err != nil {
		t.Fatalf("seed %s as %s: %v", nsPath, typ, err)
	}
	return local
}

// seedPromoteLocalGit seeds a local_git project whose directory is a real git
// repo with commits and no remote — the NOVCS-01 population promote exists for.
func seedPromoteLocalGit(t *testing.T, home, root, nsPath string, commits int) string {
	t.Helper()
	local := seedPromoteProject(t, home, root, nsPath, "local_git", "", "")
	runGit(t, local, "init", "-b", "main")
	runGit(t, local, "config", "user.name", "devstrap-test")
	runGit(t, local, "config", "user.email", "devstrap@example.test")
	for i := 0; i < commits; i++ {
		if err := os.WriteFile(filepath.Join(local, "f"+string(rune('a'+i))+".txt"), []byte("v\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, local, "add", "-A")
		runGit(t, local, "commit", "-m", "commit")
	}
	return local
}

// bareRemote creates an empty bare repository usable as a promotion target.
func bareRemote(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	runGit(t, dir, "init", "--bare", "-b", "main")
	return dir
}

func promoteProjectType(t *testing.T, home, nsPath string) state.ProjectStatus {
	t.Helper()
	store, err := state.Open(t.Context(), filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	project, err := store.ProjectByPath(t.Context(), nsPath)
	if err != nil {
		t.Fatalf("ProjectByPath(%s): %v", nsPath, err)
	}
	return project
}

// countProjectUpdatedEvents counts locally emitted project.updated events, so a
// refusal can be proven to have mutated nothing at all — not merely to have
// left the type alone.
func countProjectUpdatedEvents(t *testing.T, home string) int {
	t.Helper()
	store, err := state.Open(t.Context(), filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	events, err := store.PendingEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range events {
		if e.Type == dssync.EventProjectUpdated {
			n++
		}
	}
	return n
}

// TestPromoteTypeLattice walks every (source type, flag) pair, including every
// refusal. The type lattice is four-valued, not the two-state model the command
// surface suggests, and each cell here is a distinct decision.
func TestPromoteTypeLattice(t *testing.T) {
	cases := []struct {
		name     string
		srcType  string // "" means seed a real local_git repo on disk
		flag     string // --draft or --git-remote
		wantType string
		wantErr  string // substring; empty means success
	}{
		{name: "plain_folder to draft", srcType: "plain_folder", flag: "--draft", wantType: "draft_project"},
		{name: "draft stays draft", srcType: "draft_project", flag: "--draft", wantType: "draft_project"},
		{name: "local_git refuses draft", srcType: "local_git-repo", flag: "--draft", wantType: "local_git",
			wantErr: "--draft promotes a plain_folder only"},
		{name: "git_repo refuses draft", srcType: "git_repo", flag: "--draft", wantType: "git_repo",
			wantErr: "devstrap scan --adopt"},
		{name: "plain_folder to git_repo", srcType: "plain_folder", flag: "--git-remote", wantType: "git_repo"},
		{name: "draft to git_repo", srcType: "draft_project", flag: "--git-remote", wantType: "git_repo"},
		{name: "local_git to git_repo", srcType: "local_git-repo", flag: "--git-remote", wantType: "git_repo"},
		{name: "git_repo refuses git-remote", srcType: "git_repo", flag: "--git-remote", wantType: "git_repo",
			wantErr: "devstrap scan --adopt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home, root := promoteEnv(t)
			const nsPath = "work/subject"
			switch tc.srcType {
			case "local_git-repo":
				seedPromoteLocalGit(t, home, root, nsPath, 1)
			case "git_repo":
				seedPromoteProject(t, home, root, nsPath, "git_repo",
					"https://example.test/org/repo.git", "example.test/org/repo")
			default:
				local := seedPromoteProject(t, home, root, nsPath, tc.srcType, "", "")
				if err := os.WriteFile(filepath.Join(local, "note.md"), []byte("hi\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := countProjectUpdatedEvents(t, home)

			args := []string{"--home", home, "--root", root, "promote", nsPath}
			if tc.flag == "--draft" {
				args = append(args, "--draft")
			} else {
				args = append(args, "--git-remote", bareRemote(t, "remote.git"))
			}
			stdout, stderr, err := executeForTest(args...)

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("promote failed: stdout = %q stderr = %q err = %v", stdout, stderr, err)
				}
			} else {
				if err == nil {
					t.Fatalf("promote succeeded, want refusal containing %q (stdout = %q)", tc.wantErr, stdout)
				}
				if !strings.Contains(stderr, tc.wantErr) {
					t.Fatalf("stderr = %q, want it to contain %q", stderr, tc.wantErr)
				}
				// A refusal must mutate nothing: no event, and for the
				// non-git sources no repository conjured on disk either.
				if after := countProjectUpdatedEvents(t, home); after != before {
					t.Fatalf("refusal emitted %d project.updated event(s)", after-before)
				}
			}
			if got := promoteProjectType(t, home, nsPath).Type; got != tc.wantType {
				t.Fatalf("stored type = %q, want %q", got, tc.wantType)
			}
		})
	}
}

// TestPromoteLocalGitPushesExistingHistory is the finding this whole command
// turns on: a local_git is a repository the user simply never pushed, so its
// EXISTING history must reach the new remote. An implementation that ran
// `git init` over it would produce a remote with one fresh commit and silently
// destroy everything before it.
func TestPromoteLocalGitPushesExistingHistory(t *testing.T) {
	home, root := promoteEnv(t)
	const nsPath = "work/unpushed"
	local := seedPromoteLocalGit(t, home, root, nsPath, 3)
	wantTip := strings.TrimSpace(runGitOutput(t, local, "rev-parse", "HEAD"))
	wantCount := strings.TrimSpace(runGitOutput(t, local, "rev-list", "--count", "HEAD"))
	if wantCount != "3" {
		t.Fatalf("fixture has %s commits, want 3", wantCount)
	}

	remote := bareRemote(t, "unpushed.git")
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "promote", nsPath, "--git-remote", remote)
	if err != nil {
		t.Fatalf("promote stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if !strings.Contains(stdout, "local_git -> git_repo") {
		t.Fatalf("stdout = %q, want the local_git -> git_repo transition", stdout)
	}

	gotTip := strings.TrimSpace(runGitOutput(t, remote, "rev-parse", "main"))
	if gotTip != wantTip {
		t.Fatalf("remote tip = %s, want the local tip %s (history was not pushed)", gotTip, wantTip)
	}
	gotCount := strings.TrimSpace(runGitOutput(t, remote, "rev-list", "--count", "main"))
	if gotCount != wantCount {
		t.Fatalf("remote commit count = %s, want %s (existing history was replaced, not pushed)", gotCount, wantCount)
	}

	project := promoteProjectType(t, home, nsPath)
	if project.Type != "git_repo" || project.RemoteURL != remote {
		t.Fatalf("project = %+v, want git_repo tracking %s", project, remote)
	}
}

// TestPromoteFailedPushLeavesProjectUnchanged pins the ordering requirement:
// validate -> push -> record. Recording first would publish a git_repo whose
// remote holds no commits, and every other device would then try to clone it.
//
// The remote is a NON-BARE repository with `main` checked out: `ls-remote`
// succeeds and reports it empty (so the preflight passes), and the push is then
// refused by the receiving end — a deterministic failure at exactly the step
// under test, unlike an unreachable URL which never gets past the preflight.
func TestPromoteFailedPushLeavesProjectUnchanged(t *testing.T) {
	home, root := promoteEnv(t)
	const nsPath = "work/unpushable"
	local := seedPromoteLocalGit(t, home, root, nsPath, 2)
	before := countProjectUpdatedEvents(t, home)

	target := filepath.Join(t.TempDir(), "checked-out")
	runGit(t, target, "init", "-b", "main")

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "promote", nsPath, "--git-remote", target)
	if err == nil {
		t.Fatalf("promote succeeded against a rejecting remote (stdout = %q)", stdout)
	}
	if !strings.Contains(stderr, "push main to") {
		t.Fatalf("stderr = %q, want the failure to name the push step", stderr)
	}

	if got := promoteProjectType(t, home, nsPath).Type; got != "local_git" {
		t.Fatalf("stored type = %q, want local_git (a failed push must not promote)", got)
	}
	if after := countProjectUpdatedEvents(t, home); after != before {
		t.Fatalf("failed push emitted %d project.updated event(s)", after-before)
	}
	// The rollback must also restore the working tree, or a retry after fixing
	// the remote would refuse on the origin this attempt left behind.
	if out := runGitOutput(t, local, "remote"); strings.TrimSpace(out) != "" {
		t.Fatalf("remotes after failed promote = %q, want none", out)
	}
}

// TestPromoteFailedPushRemovesTheRepositoryItCreated is the same rollback
// contract for the non-git sources, where the command created `.git` itself.
func TestPromoteFailedPushRemovesTheRepositoryItCreated(t *testing.T) {
	home, root := promoteEnv(t)
	const nsPath = "work/folder"
	local := seedPromoteProject(t, home, root, nsPath, "plain_folder", "", "")
	if err := os.WriteFile(filepath.Join(local, "note.md"), []byte("hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "checked-out")
	runGit(t, target, "init", "-b", "main")

	if _, _, err := executeForTest("--home", home, "--root", root, "promote", nsPath, "--git-remote", target); err == nil {
		t.Fatal("promote succeeded against a rejecting remote")
	}
	if _, err := os.Stat(filepath.Join(local, ".git")); !os.IsNotExist(err) {
		t.Fatalf("stat .git = %v, want it removed by the rollback", err)
	}
	if _, err := os.Stat(filepath.Join(local, "note.md")); err != nil {
		t.Fatalf("the user's own file did not survive the rollback: %v", err)
	}
	if got := promoteProjectType(t, home, nsPath).Type; got != "plain_folder" {
		t.Fatalf("stored type = %q, want plain_folder", got)
	}
}

// TestPromoteRefusesNonEmptyRemote: a remote that already holds refs is an
// `add` case. Pushing an unrelated history into it is the failure spec/00's
// "never adopted as broken clonable git repos" promise exists to prevent.
func TestPromoteRefusesNonEmptyRemote(t *testing.T) {
	home, root := promoteEnv(t)
	const nsPath = "work/collide"
	seedPromoteLocalGit(t, home, root, nsPath, 1)

	remote := bareRemote(t, "occupied.git")
	seed := filepath.Join(t.TempDir(), "seed")
	runGit(t, seed, "init", "-b", "main")
	runGit(t, seed, "config", "user.name", "devstrap-test")
	runGit(t, seed, "config", "user.email", "devstrap@example.test")
	if err := os.WriteFile(filepath.Join(seed, "other.txt"), []byte("other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "add", "-A")
	runGit(t, seed, "commit", "-m", "occupying history")
	runGit(t, seed, "remote", "add", "origin", remote)
	runGit(t, seed, "-c", "protocol.file.allow=always", "push", "origin", "main")

	before := countProjectUpdatedEvents(t, home)
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "promote", nsPath, "--git-remote", remote)
	if err == nil {
		t.Fatalf("promote succeeded into a non-empty remote (stdout = %q)", stdout)
	}
	if !strings.Contains(stderr, "already has refs") || !strings.Contains(stderr, "devstrap scan --adopt") {
		t.Fatalf("stderr = %q, want a refusal naming the existing refs and `devstrap scan --adopt`", stderr)
	}
	if got := promoteProjectType(t, home, nsPath).Type; got != "local_git" {
		t.Fatalf("stored type = %q, want local_git", got)
	}
	if after := countProjectUpdatedEvents(t, home); after != before {
		t.Fatalf("refusal emitted %d project.updated event(s)", after-before)
	}
}

// TestPromoteRefusalsThatProtectContent covers the remaining refusals whose
// absence would each ship a specific, silent data problem.
func TestPromoteRefusalsThatProtectContent(t *testing.T) {
	t.Run("empty folder gets no invented commit", func(t *testing.T) {
		home, root := promoteEnv(t)
		seedPromoteProject(t, home, root, "work/empty", "plain_folder", "", "")
		_, stderr, err := executeForTest("--home", home, "--root", root, "promote", "work/empty", "--git-remote", bareRemote(t, "e.git"))
		if err == nil {
			t.Fatal("promote succeeded on an empty folder")
		}
		if !strings.Contains(stderr, "is empty") {
			t.Fatalf("stderr = %q, want an empty-folder refusal", stderr)
		}
	})

	t.Run("secret-looking files are not pushed to a remote", func(t *testing.T) {
		home, root := promoteEnv(t)
		local := seedPromoteProject(t, home, root, "work/secrets", "plain_folder", "", "")
		if err := os.WriteFile(filepath.Join(local, ".env"), []byte("TOKEN=abc\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, stderr, err := executeForTest("--home", home, "--root", root, "promote", "work/secrets", "--git-remote", bareRemote(t, "s.git"))
		if err == nil {
			t.Fatal("promote committed a secret-looking file")
		}
		if !strings.Contains(stderr, "secret-looking") || !strings.Contains(stderr, ".env") {
			t.Fatalf("stderr = %q, want the refusal to name the file", stderr)
		}
		if _, err := os.Stat(filepath.Join(local, ".git")); !os.IsNotExist(err) {
			t.Fatalf("stat .git = %v, want the refusal to leave no repository behind", err)
		}
	})

	// A nested repository is the one thing `git add -A` does NOT descend into:
	// it stages a gitlink (mode 160000), so the pushed tree names a commit whose
	// objects the remote never receives and no .gitmodules resolves. Git's own
	// "adding embedded git repository" warning goes to stderr, which Runner.Run
	// drops, so without this refusal the promotion looks like it succeeded and
	// the nested content is simply gone when the project materializes elsewhere
	// (P11-PROMOTE-03).
	t.Run("a nested git repository is refused, not committed as a gitlink", func(t *testing.T) {
		home, root := promoteEnv(t)
		local := seedPromoteProject(t, home, root, "work/nested", "plain_folder", "", "")
		if err := os.WriteFile(filepath.Join(local, "top.md"), []byte("top\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		inner := filepath.Join(local, "vendor", "inner")
		if err := os.MkdirAll(inner, 0o755); err != nil {
			t.Fatal(err)
		}
		runGit(t, inner, "init", "-b", "main")
		runGit(t, inner, "config", "user.name", "devstrap-test")
		runGit(t, inner, "config", "user.email", "devstrap@example.test")
		if err := os.WriteFile(filepath.Join(inner, "lib.go"), []byte("package lib\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, inner, "add", "-A")
		// Without a commit the nested repo stages nothing at all, so the gitlink
		// — the thing being refused — would never appear.
		runGit(t, inner, "commit", "-m", "inner")

		_, stderr, err := executeForTest("--home", home, "--root", root, "promote", "work/nested", "--git-remote", bareRemote(t, "n.git"))
		if err == nil {
			t.Fatal("promote committed a nested git repository as a gitlink")
		}
		if !strings.Contains(stderr, "gitlink") || !strings.Contains(stderr, "vendor/inner") {
			t.Fatalf("stderr = %q, want a gitlink refusal naming vendor/inner", stderr)
		}
		if _, err := os.Stat(filepath.Join(local, ".git")); !os.IsNotExist(err) {
			t.Fatalf("stat .git = %v, want the refusal to leave no repository behind", err)
		}
		if got := promoteProjectType(t, home, "work/nested").Type; got != "plain_folder" {
			t.Fatalf("stored type = %q, want plain_folder", got)
		}
	})

	// P11-PROMOTE-02 armed the .git rollback BEFORE `git init` rather than
	// after, because init creates and populates .git incrementally: a failure
	// partway through leaves a partial .git that wedges every later attempt.
	//
	// What this subtest does NOT prove: that partial state. Making git fail
	// mid-init is not portably reproducible (it needs ENOSPC or a fault injected
	// between two of git's own syscalls). It drives the reachable half — an init
	// that fails having created nothing — which is precisely the case where
	// arming the rollback earlier could have caused harm, and proves it does
	// not: the only path the rollback removes is one this call created.
	t.Run("a failed init leaves the folder exactly as it was", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores the directory mode this test depends on")
		}
		home, root := promoteEnv(t)
		local := seedPromoteProject(t, home, root, "work/readonly", "plain_folder", "", "")
		if err := os.WriteFile(filepath.Join(local, "keep.md"), []byte("keep\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		// Read+execute but not write: `git init` cannot create .git at all.
		if err := os.Chmod(local, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(local, 0o700) })

		_, stderr, err := executeForTest("--home", home, "--root", root, "promote", "work/readonly", "--git-remote", bareRemote(t, "ro.git"))
		if err == nil {
			t.Fatalf("promote succeeded in a directory git cannot write to (stderr = %q)", stderr)
		}
		// The refusal must come from the init itself — an earlier gate failing
		// would leave this subtest proving nothing about the rollback path.
		if !strings.Contains(stderr, "init -b") {
			t.Fatalf("stderr = %q, want the failure to come from `git init`", stderr)
		}
		if err := os.Chmod(local, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(local, ".git")); !os.IsNotExist(err) {
			t.Fatalf("stat .git = %v, want no repository left behind", err)
		}
		if _, err := os.Stat(filepath.Join(local, "keep.md")); err != nil {
			t.Fatalf("stat keep.md = %v, want the user's file untouched by the rollback", err)
		}
		if got := promoteProjectType(t, home, "work/readonly").Type; got != "plain_folder" {
			t.Fatalf("stored type = %q, want plain_folder", got)
		}
	})

	// `dsgit.IsRepo` resolves symlinks (os.Stat), so a DANGLING `.git` symlink
	// reads as "not a repository" and reaches promoteInitRepo. Left alone,
	// `git init` FOLLOWS the link and initializes the repository at its target —
	// anywhere on the disk, outside everything VerifyWithinRoot checked — and
	// the rollback armed for P11-PROMOTE-02 would then delete a node the user
	// created rather than one this command did. Both are refused by the Lstat
	// gate; this pins both halves.
	t.Run("a dangling .git symlink is refused, not initialized through", func(t *testing.T) {
		home, root := promoteEnv(t)
		local := seedPromoteProject(t, home, root, "work/symlink", "plain_folder", "", "")
		if err := os.WriteFile(filepath.Join(local, "keep.md"), []byte("keep\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "escaped")
		if err := os.Symlink(outside, filepath.Join(local, ".git")); err != nil {
			t.Fatal(err)
		}

		_, stderr, err := executeForTest("--home", home, "--root", root, "promote", "work/symlink", "--git-remote", bareRemote(t, "sym.git"))
		if err == nil {
			t.Fatal("promote initialized a repository through a dangling .git symlink")
		}
		if !strings.Contains(stderr, "not a usable repository") {
			t.Fatalf("stderr = %q, want the unusable-.git refusal", stderr)
		}
		if _, err := os.Lstat(filepath.Join(local, ".git")); err != nil {
			t.Fatalf("lstat .git = %v, want the user's own symlink left in place", err)
		}
		if _, err := os.Stat(outside); !os.IsNotExist(err) {
			t.Fatalf("stat %s = %v, want NO repository initialized outside the managed root", outside, err)
		}
	})

	t.Run("a pre-existing origin is never rewritten", func(t *testing.T) {
		home, root := promoteEnv(t)
		// scan classifies a repo whose origin FAILED validation as local_git
		// too (NOVCS-01), so a local_git can already carry an origin.
		local := seedPromoteLocalGit(t, home, root, "work/hasorigin", 1)
		runGit(t, local, "remote", "add", "origin", "https://example.test/old/repo.git")
		_, stderr, err := executeForTest("--home", home, "--root", root, "promote", "work/hasorigin", "--git-remote", bareRemote(t, "n.git"))
		if err == nil {
			t.Fatal("promote rewrote an existing origin")
		}
		if !strings.Contains(stderr, "already has an 'origin' remote") {
			t.Fatalf("stderr = %q, want the existing-origin refusal", stderr)
		}
		if got := strings.TrimSpace(runGitOutput(t, local, "remote", "get-url", "origin")); got != "https://example.test/old/repo.git" {
			t.Fatalf("origin = %q, want it untouched", got)
		}
	})

	t.Run("both flags at once is a usage error", func(t *testing.T) {
		home, root := promoteEnv(t)
		seedPromoteProject(t, home, root, "work/x", "plain_folder", "", "")
		_, stderr, err := executeForTest("--home", home, "--root", root, "promote", "work/x", "--draft", "--git-remote", "https://example.test/a.git")
		if err == nil {
			t.Fatal("promote accepted both flags")
		}
		if !strings.Contains(stderr, "mutually exclusive") {
			t.Fatalf("stderr = %q, want a mutual-exclusion usage error", stderr)
		}
	})

	t.Run("an unvalidatable remote is refused by the shared helper", func(t *testing.T) {
		home, root := promoteEnv(t)
		seedPromoteLocalGit(t, home, root, "work/badremote", 1)
		_, stderr, err := executeForTest("--home", home, "--root", root, "promote", "work/badremote",
			"--git-remote", "ext::sh -c 'echo pwned'")
		if err == nil {
			t.Fatal("promote accepted an ext:: remote")
		}
		if !strings.Contains(stderr, "unsupported git remote scheme") {
			t.Fatalf("stderr = %q, want the shared ValidateRemote refusal", stderr)
		}
	})
}

// TestPromoteDraftEmitsProjectUpdated proves the --draft path records the
// transition through the existing event kind, with content left to the shipped
// draft-bundle plane rather than a second bundling path.
func TestPromoteDraftEmitsProjectUpdated(t *testing.T) {
	home, root := promoteEnv(t)
	const nsPath = "work/notes"
	local := seedPromoteProject(t, home, root, nsPath, "plain_folder", "", "")
	if err := os.WriteFile(filepath.Join(local, "notes.md"), []byte("hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := countProjectUpdatedEvents(t, home)

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "promote", nsPath, "--draft")
	if err != nil {
		t.Fatalf("promote stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if !strings.Contains(stdout, "-> draft_project") {
		t.Fatalf("stdout = %q, want the draft transition", stdout)
	}
	if got := countProjectUpdatedEvents(t, home); got != before+1 {
		t.Fatalf("emitted %d project.updated event(s), want exactly 1", got-before)
	}
	if got := promoteProjectType(t, home, nsPath).Type; got != "draft_project" {
		t.Fatalf("stored type = %q, want draft_project", got)
	}

	// The draft plane must now accept it — the promotion is only useful if the
	// shipped bundling path takes over from here.
	if stdout, stderr, err := executeForTest("--home", home, "--root", root, "draft", "snapshot", "create", nsPath); err != nil {
		t.Fatalf("draft snapshot create after promote: stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}

	// Re-running --draft is an idempotent no-op: no second event.
	if _, _, err := executeForTest("--home", home, "--root", root, "promote", nsPath, "--draft"); err != nil {
		t.Fatalf("re-promote failed: %v", err)
	}
	if got := countProjectUpdatedEvents(t, home); got != before+1 {
		t.Fatalf("re-running --draft emitted %d extra event(s), want 0", got-before-1)
	}
}

// TestPromoteRemediesNameCommandsThatWorkInTheStateTheyAreOffered pins a whole
// property of this file, not one instance of it. `devstrap add` calls
// ensureHydratableTarget (add.go), which refuses any directory that is neither
// empty nor a skeleton — and EVERY state promote can refuse from has a
// populated directory by construction (a local_git has commits, a git_repo is a
// materialized checkout, a plain_folder that reached the push preflight has
// content). So `add` is never a runnable remedy in a promote refusal, and the
// correct one is `scan --adopt`, which upserts by path from the checkout's own
// origin.
//
// The original version of this test asserted the general property in its name
// while checking a single 400-byte window around one message, and two other
// messages named `devstrap add` unnoticed for exactly that reason
// (P11-PROMOTE-01). It now walks every string literal in the file.
//
// Scope is string literals, deliberately: `devstrap add` must remain sayable in
// a COMMENT, since the comments explaining why `add` is wrong have to name it.
// A literal is user-facing text — help output or an error — and there is no
// legitimate occurrence there, so the allowlist below is empty rather than
// absent; a future empty-directory remedy would be added to it with a reason.
//
// This asserts on the messages rather than driving each failure, because
// forcing a store write to fail after a successful push needs a fault-injection
// seam this package does not have. Naming the wrong command is the defect; the
// message is where it lives.
func TestPromoteRemediesNameCommandsThatWorkInTheStateTheyAreOffered(t *testing.T) {
	// allowedAddText holds any user-facing string legitimately naming
	// `devstrap add` — one offered only in an empty-directory state, where add
	// would actually run. There are none today.
	allowedAddText := map[string]string{}

	texts := promoteUserFacingStrings(t)
	// Guard the collection itself: an empty or near-empty result would make
	// every assertion below pass while proving nothing.
	if len(texts) < 20 {
		t.Fatalf("collected only %d user-facing strings from promote.go; the walk is not reaching the file's text", len(texts))
	}
	for _, text := range texts {
		if !strings.Contains(text.value, "devstrap add") {
			continue
		}
		if reason, allowed := allowedAddText[text.value]; allowed {
			t.Logf("allowed `devstrap add` at %s: %s", text.pos, reason)
			continue
		}
		t.Errorf("%s: user-facing text names `devstrap add`, which refuses a non-empty, "+
			"non-skeleton directory (add.go -> ensureHydratableTarget) — every state promote "+
			"refuses from has a populated directory, so the remedy cannot run where it is "+
			"offered. Use `devstrap scan --adopt`:\n\t%q", text.pos, text.value)
	}

	// The check above passes trivially if the remedies are deleted rather than
	// corrected, so each refusal that used to name `add` must positively name
	// the full `devstrap scan --adopt`. Both halves are asserted against the
	// SAME string value, so an anchor cannot match one message while the remedy
	// it is credited with lives in a neighbouring comment or literal.
	for _, remedy := range []struct {
		what   string
		anchor string
	}{
		{what: "the push-succeeded/record-failed recovery", anchor: "but recording it failed"},
		{what: "the already-a-git_repo refusal", anchor: "promote only graduates remote-less projects"},
		{what: "the non-empty-remote refusal", anchor: "promote pushes into an EMPTY remote only"},
		{what: "the non-empty-remote help text", anchor: "a remote that already holds refs"},
	} {
		found := false
		for _, text := range texts {
			if !strings.Contains(text.value, remedy.anchor) {
				continue
			}
			found = true
			if !strings.Contains(text.value, "devstrap scan --adopt") {
				t.Errorf("%s (%s) should name `devstrap scan --adopt`, which adopts an existing "+
					"checkout whose origin validates:\n\t%q", remedy.what, text.pos, text.value)
			}
		}
		if !found {
			t.Errorf("could not find %s (anchor %q) in any user-facing string; update this test if it was reworded",
				remedy.what, remedy.anchor)
		}
	}
}

// promoteUserFacingString is one string value promote.go can emit, with the
// position it came from.
type promoteUserFacingString struct {
	pos   string
	value string
}

// promoteUserFacingStrings returns every string promote.go can put in front of
// a user: each string literal, plus the folded value of each `+` concatenation
// of literals.
//
// Folding the concatenations is not incidental. This file wraps its long
// messages across several `+`-joined literals, so a check that only looked at
// individual literals could be defeated by a wrap that happened to split the
// offending phrase — `"use `devstrap " + "add` here"` contains it in neither
// half. Comments are excluded by construction (they are not in the AST at
// this parse mode), and deliberately so: the comments explaining why `add` is
// the wrong remedy have to be able to name it.
func promoteUserFacingStrings(t *testing.T) []promoteUserFacingString {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "promote.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	// fold returns the constant string an expression evaluates to, if it is
	// built only from string literals and `+`.
	var fold func(ast.Expr) (string, bool)
	fold = func(expr ast.Expr) (string, bool) {
		switch e := expr.(type) {
		case *ast.BasicLit:
			if e.Kind != token.STRING {
				return "", false
			}
			value, err := strconv.Unquote(e.Value)
			if err != nil {
				// A STRING literal in a file the parser accepted must unquote;
				// failing open here would silently drop text from the scan.
				t.Fatalf("%s: could not unquote string literal %s: %v", fset.Position(e.Pos()), e.Value, err)
			}
			return value, true
		case *ast.ParenExpr:
			return fold(e.X)
		case *ast.BinaryExpr:
			if e.Op != token.ADD {
				return "", false
			}
			left, ok := fold(e.X)
			if !ok {
				return "", false
			}
			right, ok := fold(e.Y)
			if !ok {
				return "", false
			}
			return left + right, true
		}
		return "", false
	}

	var texts []promoteUserFacingString
	ast.Inspect(file, func(n ast.Node) bool {
		expr, ok := n.(ast.Expr)
		if !ok {
			return true
		}
		value, ok := fold(expr)
		if !ok {
			return true
		}
		texts = append(texts, promoteUserFacingString{pos: fset.Position(expr.Pos()).String(), value: value})
		// Stop at the OUTERMOST expression that folds. Descending would also
		// record every partial concatenation, and a prefix that happens to end
		// mid-message would then be judged as if it were the whole message —
		// an anchor without the remedy that follows it two literals later.
		return false
	})
	return texts
}
