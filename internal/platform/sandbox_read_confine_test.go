package platform

import (
	"runtime"
	"strings"
	"testing"
)

func TestReadConfineRoots(t *testing.T) {
	spec := SandboxSpec{
		WorktreeDir:    "/work/tree",
		TmpDir:         "/tmp/run",
		UserHome:       "/home/dev",
		ReadAllowExtra: []string{"/opt/extra", "relative/skip", "/opt/extra"},
	}
	roots := readConfineRoots(spec)
	set := make(map[string]bool, len(roots))
	for _, r := range roots {
		if set[r] {
			t.Fatalf("duplicate root %q in %v", r, roots)
		}
		set[r] = true
	}
	// worktree + tmp are always roots (they are also read-write).
	for _, want := range []string{"/work/tree", "/tmp/run"} {
		if !set[want] {
			t.Fatalf("missing root %q in %v", want, roots)
		}
	}
	// $HOME build caches present; credential dirs absent.
	if !set["/home/dev/.cache"] || !set["/home/dev/go"] {
		t.Fatalf("missing home cache roots in %v", roots)
	}
	for _, cred := range []string{"/home/dev/.ssh", "/home/dev/.aws", "/home/dev/.gnupg", "/home/dev/.config"} {
		if set[cred] {
			t.Fatalf("credential path %q must NOT be a read root: %v", cred, roots)
		}
	}
	// Absolute extra kept, deduped; relative extra dropped.
	if !set["/opt/extra"] {
		t.Fatalf("absolute --read-allow extra missing: %v", roots)
	}
	for _, r := range roots {
		if strings.Contains(r, "relative/skip") {
			t.Fatalf("relative --read-allow must be dropped: %v", roots)
		}
	}
	// OS system roots match the running platform.
	wantSys := "/usr"
	if !set[wantSys] {
		t.Fatalf("missing system root %q in %v", wantSys, roots)
	}
	if runtime.GOOS == "darwin" && !set["/System"] {
		t.Fatalf("darwin system roots missing /System: %v", roots)
	}
	if runtime.GOOS == "linux" && !set["/lib"] {
		t.Fatalf("linux system roots missing /lib: %v", roots)
	}
}

func TestFirstReadAllowCredentialConflict(t *testing.T) {
	home := "/home/dev"
	devstrap := "/home/dev/.devstrap"
	cases := []struct {
		name      string
		readAllow []string
		want      string
	}{
		{name: "clean extras", readAllow: []string{"/opt/data", "/srv/cache"}, want: ""},
		{name: "filesystem root overlaps everything", readAllow: []string{"/"}, want: "/"},
		{name: "exact credential dir", readAllow: []string{"/home/dev/.ssh"}, want: "/home/dev/.ssh"},
		{name: "ancestor of credentials (whole home)", readAllow: []string{"/home/dev"}, want: "/home/dev"},
		{name: "inside a credential dir", readAllow: []string{"/home/dev/.ssh/keys"}, want: "/home/dev/.ssh/keys"},
		{name: "devstrap keys", readAllow: []string{"/home/dev/.devstrap/keys"}, want: "/home/dev/.devstrap/keys"},
		{name: "credential file", readAllow: []string{"/home/dev/.netrc"}, want: "/home/dev/.netrc"},
		{name: "sibling of a credential is fine", readAllow: []string{"/home/dev/.sshkeys"}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FirstReadAllowCredentialConflict(home, devstrap, tc.readAllow); got != tc.want {
				t.Fatalf("FirstReadAllowCredentialConflict = %q, want %q", got, tc.want)
			}
		})
	}
}
