//go:build linux

package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// grantIndex builds the read-grant set for a fake $HOME and exposes membership
// helpers. credentialExcludingReadGrants is pure filesystem-walk logic (no
// Landlock syscalls), so these run on the default `go test` Linux CI leg, not
// only under the kernel-gated DEVSTRAP_SANDBOX_E2E enforcement test.
type grantIndex struct {
	t      *testing.T
	byPath map[string]readGrant
}

func indexGrants(t *testing.T, home string) grantIndex {
	t.Helper()
	idx := grantIndex{t: t, byPath: map[string]readGrant{}}
	grants, err := credentialExcludingReadGrants(home, "")
	if err != nil {
		t.Fatalf("credentialExcludingReadGrants(%q, \"\"): %v", home, err)
	}
	for _, g := range grants {
		idx.byPath[g.path] = g
	}
	return idx
}

func (g grantIndex) granted(path string) bool {
	_, ok := g.byPath[path]
	return ok
}

// grantedUnder reports whether any grant path lies strictly inside dir.
func (g grantIndex) grantedUnder(dir string) bool {
	prefix := dir + string(os.PathSeparator)
	for p := range g.byPath {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

func (g grantIndex) mustGrantDir(path string) {
	g.t.Helper()
	got, ok := g.byPath[path]
	if !ok {
		g.t.Errorf("expected %q to be read-granted, but it was not", path)
		return
	}
	if !got.dir {
		g.t.Errorf("expected %q to be granted as a directory (RODirs), got file grant", path)
	}
}

func (g grantIndex) mustNotGrant(path string) {
	g.t.Helper()
	if g.granted(path) {
		g.t.Errorf("expected %q NOT to be read-granted (credential/ancestor), but it was", path)
	}
}

// mustResolvedTempHome returns t.TempDir() with symlinks resolved, so the paths
// the walk produces (it descends the real filesystem from "/") match the paths
// the test constructs and asserts on.
func mustResolvedTempHome(t *testing.T) string {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return home
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func mkfile(t *testing.T, path string) {
	t.Helper()
	mkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestCredentialExcludingReadGrantsNestedAnchor pins the recursion: a credential
// anchor two levels deep (.config/gh) must be carved out while siblings at every
// level of its ancestor chain stay granted, and the ancestor directories
// themselves must be descended into, never granted wholesale.
func TestCredentialExcludingReadGrantsNestedAnchor(t *testing.T) {
	home := mustResolvedTempHome(t)
	ghAnchor := filepath.Join(home, ".config", "gh")
	mkfile(t, filepath.Join(ghAnchor, "hosts.yml"))
	otherTool := filepath.Join(home, ".config", "other-tool")
	mkdir(t, otherTool)
	projects := filepath.Join(home, "projects")
	mkdir(t, projects)

	idx := indexGrants(t, home)

	// The nested anchor and everything under it: no grant.
	idx.mustNotGrant(ghAnchor)
	if idx.grantedUnder(ghAnchor) {
		t.Errorf("a path inside the credential anchor %q was granted", ghAnchor)
	}
	// The ancestor directories are recursed, not granted wholesale — granting
	// $HOME or $HOME/.config as a leaf would re-expose the anchor beneath it.
	idx.mustNotGrant(home)
	idx.mustNotGrant(filepath.Join(home, ".config"))
	// Siblings at the anchor's level and above are granted normally.
	idx.mustGrantDir(otherTool)
	idx.mustGrantDir(projects)
}

// TestCredentialExcludingReadGrantsSiblingSymlinkAlias pins the grantLeaf
// anti-aliasing skip: a symlink sibling whose target resolves inside an anchor
// is dropped, while a symlink to a non-credential target is granted.
func TestCredentialExcludingReadGrantsSiblingSymlinkAlias(t *testing.T) {
	home := mustResolvedTempHome(t)
	ghAnchor := filepath.Join(home, ".config", "gh")
	mkfile(t, filepath.Join(ghAnchor, "hosts.yml"))
	projects := filepath.Join(home, "projects")
	mkdir(t, projects)

	// Sibling of the anchor inside the walked .config dir, aliasing the anchor.
	ghAlias := filepath.Join(home, ".config", "gh-alias")
	if err := os.Symlink(ghAnchor, ghAlias); err != nil {
		t.Fatal(err)
	}
	// Sibling symlink whose target is NOT a credential path.
	safeLink := filepath.Join(home, ".config", "safe-link")
	if err := os.Symlink(projects, safeLink); err != nil {
		t.Fatal(err)
	}

	idx := indexGrants(t, home)

	idx.mustNotGrant(ghAlias) // would re-expose the anchor by alias
	idx.mustGrantDir(safeLink)
}

// TestCredentialExcludingReadGrantsSymlinkedAnchor pins Finding 1's fix: when a
// credential anchor is ITSELF a symlink to a target outside its own directory
// (the stow/chezmoi dotfiles layout, ~/.ssh -> ~/dotfiles/ssh), both the literal
// alias and the resolved target subtree must be excluded, while the target
// directory's OTHER siblings stay granted. Without the EvalSymlinks union on the
// anchor, ~/dotfiles would be granted wholesale, re-exposing ~/dotfiles/ssh.
func TestCredentialExcludingReadGrantsSymlinkedAnchor(t *testing.T) {
	home := mustResolvedTempHome(t)
	dotfilesSSH := filepath.Join(home, "dotfiles", "ssh")
	mkfile(t, filepath.Join(dotfilesSSH, "id_rsa"))
	dotfilesOther := filepath.Join(home, "dotfiles", "other")
	mkdir(t, dotfilesOther)
	projects := filepath.Join(home, "projects")
	mkdir(t, projects)

	// ~/.ssh is a symlink into the dotfiles tree.
	sshLink := filepath.Join(home, ".ssh")
	if err := os.Symlink(dotfilesSSH, sshLink); err != nil {
		t.Fatal(err)
	}

	idx := indexGrants(t, home)

	// The literal alias is denied by the by-path carve-out.
	idx.mustNotGrant(sshLink)
	// The resolved target subtree is denied by the EvalSymlinks union (the fix):
	// neither dotfiles/ssh nor anything under it may be granted, and dotfiles
	// itself must be recursed, not granted wholesale.
	idx.mustNotGrant(filepath.Join(home, "dotfiles"))
	idx.mustNotGrant(dotfilesSSH)
	if idx.grantedUnder(dotfilesSSH) {
		t.Errorf("a path inside the symlinked anchor target %q was granted", dotfilesSSH)
	}
	// The target directory's non-credential sibling and unrelated top-level dirs
	// stay granted.
	idx.mustGrantDir(dotfilesOther)
	idx.mustGrantDir(projects)
}

// TestCredentialExcludingReadGrantsNoAnchors covers the degenerate case: with no
// home and no devstrap home there are no anchors, so DenySensitiveReads has
// nothing to scope against. This must fail closed with an error, not fall
// back to a wholesale RODirs("/") grant that would silently defeat the
// caller's request.
func TestCredentialExcludingReadGrantsNoAnchors(t *testing.T) {
	grants, err := credentialExcludingReadGrants("", "")
	if err == nil {
		t.Fatalf("expected an error with no anchors, got grants: %+v", grants)
	}
	if grants != nil {
		t.Fatalf("expected nil grants on error, got: %+v", grants)
	}
}
