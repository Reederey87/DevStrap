package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func stubSSH(t *testing.T) {
	t.Helper()

	dir := t.TempDir()
	script := `#!/bin/sh
last=
for arg do
  last=$arg
done
case "$last" in
  work-gitlab)
    printf '%s\n' 'user git' 'hostname git.acme.com'
    ;;
  forge-alias)
    printf '%s\n' 'hostname gitlab.acme.com'
    ;;
  marker-alias)
    printf '%s\n' 'hostname marker.stub.invalid'
    ;;
  error-gitlab)
    echo "stub ssh forced failure" >&2
    exit 42
    ;;
  *)
    printf 'hostname %s\n' "$last"
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestParseForgeKind(t *testing.T) {
	cases := []struct {
		in   string
		want ForgeKind
		ok   bool
	}{
		{"github", ForgeGitHub, true},
		{"GitLab", ForgeGitLab, true},
		{"gitea", ForgeGitea, true},
		{"bitbucket", ForgeBitbucket, true},
		{"azure", ForgeAzure, true},
		{"", ForgeUnknown, false},
		{"bogus", ForgeUnknown, false},
		{"  ", ForgeUnknown, false},
	}
	for _, c := range cases {
		got, ok := parseForgeKind(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("parseForgeKind(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestResolveForgePrecedence(t *testing.T) {
	hostMap := map[string]ForgeKind{"git.acme.com": ForgeGitLab}
	// Flag wins over everything.
	if k := ResolveForge("https://git.acme.com/acme/repo", "gitea", "github", hostMap); k != ForgeGitea {
		t.Errorf("flag precedence: got %q, want gitea", k)
	}
	// Project column wins over host map and detection.
	if k := ResolveForge("https://git.acme.com/acme/repo", "", "github", hostMap); k != ForgeGitHub {
		t.Errorf("project precedence: got %q, want github", k)
	}
	// Host map wins over detection for a self-hosted GitLab.
	if k := ResolveForge("https://git.acme.com/acme/repo", "", "", hostMap); k != ForgeGitLab {
		t.Errorf("host-map precedence: got %q, want gitlab", k)
	}
	// Detection heuristic for a known host.
	if k := ResolveForge("git@github.com:acme/repo.git", "", "", nil); k != ForgeGitHub {
		t.Errorf("detection: got %q, want github", k)
	}
	// Unknown self-hosted with no overrides.
	if k := ResolveForge("git@scm.internal:org/repo.git", "", "", nil); k != ForgeUnknown {
		t.Errorf("unknown: got %q, want empty", k)
	}
	// Invalid flag falls through to the next tier.
	if k := ResolveForge("https://git.acme.com/acme/repo", "bogus", "", hostMap); k != ForgeGitLab {
		t.Errorf("invalid flag fallthrough: got %q, want gitlab", k)
	}
}

func TestSSHHostMatch(t *testing.T) {
	cases := []struct {
		pattern, host string
		want          bool
	}{
		{"*", "anything", true},
		{"work-gitlab", "work-gitlab", true},
		{"work-gitlab", "other", false},
		{"*.example.com", "a.example.com", true},
		{"*.example.com", "example.com", false},
		{"host?", "host1", true},
	}
	for _, c := range cases {
		if got := sshHostMatch(c.pattern, c.host); got != c.want {
			t.Errorf("sshHostMatch(%q,%q) = %v, want %v", c.pattern, c.host, got, c.want)
		}
	}
}

// P5 review: the file parser must honor OpenSSH negation so it never returns a
// host that ssh would exclude.
func TestSSHHostAliasFromFileHonorsNegation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := "Host *gitlab* !work-gitlab\n  HostName real.gitlab.com\n"
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	// work-gitlab is negated → no override.
	if got := resolveSSHHostAliasFromFile("work-gitlab"); got != "" {
		t.Errorf("resolveSSHHostAliasFromFile(work-gitlab) = %q, want empty (negated by !work-gitlab)", got)
	}
	// A non-negated sibling still matches the glob.
	if got := resolveSSHHostAliasFromFile("dev-gitlab"); got != "real.gitlab.com" {
		t.Errorf("resolveSSHHostAliasFromFile(dev-gitlab) = %q, want real.gitlab.com", got)
	}
	// P5 review: a leading-dash alias is rejected (ssh-option injection guard).
	if got := resolveSSHHostAlias("-Fmalicious"); got != "" {
		t.Errorf("resolveSSHHostAlias(-Fmalicious) = %q, want empty (leading-dash rejected)", got)
	}
}

func TestResolveSSHHostAlias(t *testing.T) {
	stubSSH(t)

	home := t.TempDir()
	t.Setenv("HOME", home)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := `# work gitlab alias
Host work-gitlab
  HostName git.acme.com
  User git

Host error-gitlab
  HostName fallback.gitlab.acme.com
  User git

Host *
  User git
`
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := resolveSSHHostAlias("work-gitlab"); got != "git.acme.com" {
		t.Errorf("resolveSSHHostAlias(work-gitlab) = %q, want git.acme.com", got)
	}
	if got := resolveSSHHostAlias("error-gitlab"); got != "fallback.gitlab.acme.com" {
		t.Errorf("resolveSSHHostAlias(error-gitlab) = %q, want fallback.gitlab.acme.com", got)
	}
	if got := resolveSSHHostAlias("other"); got != "" {
		t.Errorf("resolveSSHHostAlias(other) = %q, want empty", got)
	}
}

func TestSSHAliasResolutionUsesStub(t *testing.T) {
	stubSSH(t)
	t.Setenv("HOME", t.TempDir())

	if got := resolveSSHHostAlias("marker-alias"); got != "marker.stub.invalid" {
		t.Errorf("resolveSSHHostAlias(marker-alias) = %q, want marker.stub.invalid", got)
	}
}

func TestDetectForgeResolvesSSHAlias(t *testing.T) {
	stubSSH(t)

	home := t.TempDir()
	t.Setenv("HOME", home)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := "Host forge-alias\n  HostName gitlab.acme.com\n"
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	// The alias `forge-alias` resolves to `gitlab.acme.com` which contains
	// "gitlab." and so DetectForge returns ForgeGitLab (GIT-05).
	if got := DetectForge("git@forge-alias:org/repo.git"); got != ForgeGitLab {
		t.Errorf("DetectForge(alias) = %q, want gitlab", got)
	}
}
