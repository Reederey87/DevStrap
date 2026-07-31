package platform

import (
	"slices"
	"strings"
	"testing"
)

// gitStorageDirs are the kind of paths WorktreeSandboxWriteDirs yields: the
// linked worktree's git storage, never the common dir root or hooks/config.
var gitStorageDirs = []string{
	"/home/dev/clone/.git/objects",
	"/home/dev/clone/.git/refs",
	"/home/dev/clone/.git/logs",
	"/home/dev/clone/.git/worktrees/agent-x",
}

// TestSBPLProfileGrantsGitDirs proves the Seatbelt write allow-list includes
// each git storage dir so an agent `git commit` in a linked worktree is not
// EPERM'd (P7-SANDBOX-01) — the platform where the default-on sandbox bites.
func TestSBPLProfileGrantsGitDirs(t *testing.T) {
	spec := SandboxSpec{WorktreeDir: "/wt", TmpDir: "/tmp", GitDirs: gitStorageDirs}
	profile := sbplProfile(spec, nil, nil)
	for _, d := range gitStorageDirs {
		if !strings.Contains(profile, `(subpath "`+d+`")`) {
			t.Errorf("SBPL write allow-list missing git dir %q\n%s", d, profile)
		}
	}
	// The common dir root must never be granted wholesale (hooks/config escape).
	if strings.Contains(profile, `(subpath "/home/dev/clone/.git")`) {
		t.Error("SBPL grants the whole common dir — hooks/config sandbox escape")
	}
}

// TestBwrapArgsGrantsGitDirs proves the bubblewrap write binds include each git
// storage dir (via --bind-try, tolerating an absent reflog).
func TestBwrapArgsGrantsGitDirs(t *testing.T) {
	spec := SandboxSpec{WorktreeDir: "/wt", TmpDir: "/tmp", GitDirs: gitStorageDirs}
	args := bwrapArgs(spec, nil, nil, bwrapOptions{})
	for _, d := range gitStorageDirs {
		if indexSequence(args, "--bind-try", d, d) == -1 {
			t.Errorf("bwrap args missing --bind-try %s %s\n%v", d, d, args)
		}
	}
	if slices.Contains(args, "/home/dev/clone/.git") {
		t.Error("bwrap binds the whole common dir — hooks/config sandbox escape")
	}
}

// gitDenyFiles are the kind of paths WorktreeSandboxDenyFiles yields: the
// pointer files inside the linked worktree's own admin dir (the last entry of
// gitStorageDirs), which the write-allow grants wholesale.
var gitDenyFiles = []string{
	"/home/dev/clone/.git/worktrees/agent-x/commondir",
	"/home/dev/clone/.git/worktrees/agent-x/gitdir",
	"/home/dev/clone/.git/worktrees/agent-x/config.worktree",
}

// TestSBPLProfileDeniesGitDenyFilesAfterGrantingGitDirs proves the P8-SEC-02
// fix: the SBPL profile denies writes to commondir/gitdir/config.worktree even
// though their parent admin dir is granted writable via GitDirs, AND that the
// deny appears AFTER the allow block that grants it — an SBPL deny emitted
// before the allow does nothing, since Seatbelt's file-write* matching is
// last-match-wins. A test that only greps for the deny string, without
// checking order, would pass even with the escape wide open.
func TestSBPLProfileDeniesGitDenyFilesAfterGrantingGitDirs(t *testing.T) {
	spec := SandboxSpec{WorktreeDir: "/wt", TmpDir: "/tmp", GitDirs: gitStorageDirs, GitDenyFiles: gitDenyFiles}
	profile := sbplProfile(spec, nil, nil)

	allowIdx := strings.Index(profile, `(subpath "/home/dev/clone/.git/worktrees/agent-x")`)
	if allowIdx == -1 {
		t.Fatalf("SBPL missing the write-allow grant for the per-worktree admin dir:\n%s", profile)
	}
	for _, f := range gitDenyFiles {
		denyIdx := strings.Index(profile, `(literal "`+f+`")`)
		if denyIdx == -1 {
			t.Errorf("SBPL missing deny for %q\n%s", f, profile)
			continue
		}
		if denyIdx < allowIdx {
			t.Errorf("SBPL deny for %q appears BEFORE the allow it must override (deny@%d allow@%d) — the deny does nothing under last-match-wins", f, denyIdx, allowIdx)
		}
	}
}

// TestBwrapArgsDeniesGitDenyFilesAfterGrantingGitDirs proves the bubblewrap
// equivalent: --ro-bind-try for each GitDenyFiles entry, positioned AFTER the
// --bind-try of its parent GitDirs entry — bwrap mounts apply sequentially, so
// an earlier deny would be overridden by the later read-write bind, leaving
// the file writable.
func TestBwrapArgsDeniesGitDenyFilesAfterGrantingGitDirs(t *testing.T) {
	spec := SandboxSpec{WorktreeDir: "/wt", TmpDir: "/tmp", GitDirs: gitStorageDirs, GitDenyFiles: gitDenyFiles}
	args := bwrapArgs(spec, nil, nil, bwrapOptions{})

	bindIdx := indexSequence(args, "--bind-try", "/home/dev/clone/.git/worktrees/agent-x", "/home/dev/clone/.git/worktrees/agent-x")
	if bindIdx == -1 {
		t.Fatalf("bwrap missing --bind-try for the per-worktree admin dir: %v", args)
	}
	for _, f := range gitDenyFiles {
		denyIdx := indexSequence(args, "--ro-bind-try", f, f)
		if denyIdx == -1 {
			t.Errorf("bwrap missing --ro-bind-try %s %s\n%v", f, f, args)
			continue
		}
		if denyIdx < bindIdx {
			t.Errorf("bwrap --ro-bind-try for %q appears BEFORE its parent's --bind-try (deny@%d bind@%d) — the later --bind-try would win and leave it writable", f, denyIdx, bindIdx)
		}
	}
}

// TestReadConfineRootsIncludesGitDirs proves git storage stays readable under
// --read-confine (readonly policy) so git read ops work; the RW grant already
// makes them writable elsewhere.
func TestReadConfineRootsIncludesGitDirs(t *testing.T) {
	roots := readConfineRoots(SandboxSpec{
		WorktreeDir: "/wt",
		TmpDir:      "/tmp",
		ReadConfine: true,
		GitDirs:     gitStorageDirs,
	})
	for _, d := range gitStorageDirs {
		if !slices.Contains(roots, d) {
			t.Errorf("read-confine roots missing git dir %q: %v", d, roots)
		}
	}
}
