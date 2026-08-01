package cli

import (
	"os"
	"path/filepath"
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
			wantErr: "devstrap add"},
		{name: "plain_folder to git_repo", srcType: "plain_folder", flag: "--git-remote", wantType: "git_repo"},
		{name: "draft to git_repo", srcType: "draft_project", flag: "--git-remote", wantType: "git_repo"},
		{name: "local_git to git_repo", srcType: "local_git-repo", flag: "--git-remote", wantType: "git_repo"},
		{name: "git_repo refuses git-remote", srcType: "git_repo", flag: "--git-remote", wantType: "git_repo",
			wantErr: "devstrap add"},
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
	if !strings.Contains(stderr, "already has refs") || !strings.Contains(stderr, "devstrap add") {
		t.Fatalf("stderr = %q, want a refusal naming the existing refs and `devstrap add`", stderr)
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

// TestPromoteRemediesNameCommandsThatWorkInTheStateTheyAreOffered pins the
// review's MAJOR. The push-succeeded/record-failed path used to say "re-run
// devstrap add", but `add` calls ensureHydratableTarget (add.go:73), which
// refuses a non-empty, non-skeleton directory — and in exactly that state the
// directory IS the just-promoted repository, full of the user's files. The one
// recovery command the message named failed in precisely the state the message
// was written for.
//
// This asserts on the message rather than driving the failure, because forcing
// a store write to fail after a successful push needs a fault-injection seam
// this package does not have. Naming the wrong command is the defect; the
// message is where it lives.
func TestPromoteRemediesNameCommandsThatWorkInTheStateTheyAreOffered(t *testing.T) {
	src, err := os.ReadFile("promote.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	// The recovery-from-inconsistency message must not send the user to `add`.
	const recovery = "but recording it failed"
	i := strings.Index(body, recovery)
	if i < 0 {
		t.Fatal("could not find the push-succeeded/record-failed message; update this test if it was reworded")
	}
	window := body[i : i+400]
	if strings.Contains(window, "devstrap add") {
		t.Errorf("the push-succeeded/record-failed remedy names `devstrap add`, which refuses a "+
			"non-empty directory (add.go -> ensureHydratableTarget) — i.e. it fails in exactly the "+
			"state this error leaves behind:\n%s", window)
	}
	if !strings.Contains(window, "scan --adopt") {
		t.Errorf("the recovery remedy should name `scan --adopt`, which adopts an existing checkout "+
			"whose origin validates:\n%s", window)
	}
}
