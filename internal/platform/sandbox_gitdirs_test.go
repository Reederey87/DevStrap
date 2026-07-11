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
