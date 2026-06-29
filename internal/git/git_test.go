package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCanonicalRemoteKeyNormalizesCommonGitHubForms(t *testing.T) {
	for _, remote := range []string{
		"git@github.com:org/repo.git",
		"git@github.com:2222:org/repo.git",
		"https://github.com/org/repo.git",
		"ssh://git@github.com/org/repo.git",
		"ssh://git@github.com:2222/org/repo.git",
	} {
		got, err := CanonicalRemoteKey(remote)
		if err != nil {
			t.Fatalf("CanonicalRemoteKey(%q): %v", remote, err)
		}
		if got != "github.com/org/repo" {
			t.Fatalf("CanonicalRemoteKey(%q) = %q, want github.com/org/repo", remote, got)
		}
	}
}

func TestCanonicalRemoteKeyNormalizesCaseAndTrailingSlash(t *testing.T) {
	got, err := CanonicalRemoteKey("ssh://git@GitHub.COM:2222/Org/Repo.git/")
	if err != nil {
		t.Fatal(err)
	}
	if got != "github.com/Org/Repo" {
		t.Fatalf("CanonicalRemoteKey() = %q, want host without port and trimmed path", got)
	}
}

func TestCanonicalRemoteKeySupportsLocalBareRemotes(t *testing.T) {
	got, err := CanonicalRemoteKey("file:///tmp/devstrap/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	if got != "file/tmp/devstrap/repo" {
		t.Fatalf("CanonicalRemoteKey(file URL) = %q", got)
	}
}

func TestValidateRemoteRejectsOptionAndExtProtocol(t *testing.T) {
	for _, remote := range []string{
		"--upload-pack=touch /tmp/pwned",
		"ext::sh -c touch /tmp/pwned",
		"http://github.com/org/repo.git",
		"git@github.com",
	} {
		if err := ValidateRemote(remote); err == nil {
			t.Fatalf("ValidateRemote(%q) succeeded, want error", remote)
		}
	}
}

func TestValidateRemoteAcceptsExplicitAllowlist(t *testing.T) {
	for _, remote := range []string{
		"https://github.com/org/repo.git",
		"ssh://git@github.com/org/repo.git",
		"git@github.com:org/repo.git",
		"file:///tmp/devstrap/repo.git",
		"/tmp/devstrap/repo.git",
	} {
		if err := ValidateRemote(remote); err != nil {
			t.Fatalf("ValidateRemote(%q): %v", remote, err)
		}
	}
}

func TestRedactGitTextRemovesURLCredentials(t *testing.T) {
	got := redactGitText("clone https://user:token@example.com/org/repo.git failed")
	if got != "clone https://[REDACTED]@example.com/org/repo.git failed" {
		t.Fatalf("redactGitText() = %q", got)
	}
}

func TestGitEnvUsesAllowlistAndControlledGitSettings(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("HOME", "/tmp/devstrap-home")
	t.Setenv("DEVSTRAP_TOKEN", "secret")
	t.Setenv("LD_PRELOAD", "/tmp/hook.dylib")
	t.Setenv("DYLD_INSERT_LIBRARIES", "/tmp/hook.dylib")
	t.Setenv("GIT_SSH_COMMAND", "ssh -oProxyCommand=evil")
	t.Setenv("PYTHONPATH", "/tmp/python")

	got, err := gitEnv()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"PATH=/usr/bin",
		"HOME=/tmp/devstrap-home",
		"GIT_ASKPASS=",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_PROTOCOL_FROM_USER=0",
		"GIT_TERMINAL_PROMPT=0",
		"SSH_ASKPASS=",
	} {
		if !slices.Contains(got, want) {
			t.Fatalf("gitEnv() = %#v, want %s", got, want)
		}
	}
	for _, pair := range got {
		if strings.Contains(pair, "secret") ||
			strings.HasPrefix(pair, "LD_PRELOAD=") ||
			strings.HasPrefix(pair, "DYLD_INSERT_LIBRARIES=") ||
			strings.HasPrefix(pair, "GIT_SSH_COMMAND=") ||
			strings.HasPrefix(pair, "PYTHONPATH=") {
			t.Fatalf("gitEnv() leaked blocked variable: %#v", got)
		}
	}
}

func TestSecureArgsAppliesProtocolPolicyToEveryGitInvocation(t *testing.T) {
	got := secureArgs([]string{"status", "--porcelain=v2"})
	for _, want := range []string{
		"protocol.allow=never",
		"protocol.https.allow=always",
		"protocol.ssh.allow=always",
		"protocol.git.allow=always",
		"protocol.file.allow=always",
		"protocol.ext.allow=never",
		"core.sshCommand=ssh -oBatchMode=yes",
	} {
		if !slices.Contains(got, want) {
			t.Fatalf("secureArgs() = %#v, want %s", got, want)
		}
	}
	if got[len(got)-2] != "status" || got[len(got)-1] != "--porcelain=v2" {
		t.Fatalf("secureArgs() = %#v, want original args preserved at tail", got)
	}
}

func TestClassifyGitErrorReturnsTypedSentinels(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want error
	}{
		{name: "network", msg: "fatal: unable to access 'https://example.test/repo.git/': Could not resolve host: example.test", want: ErrNetwork},
		{name: "auth", msg: "git@example.com: Permission denied (publickey). Could not read from remote repository.", want: ErrAuth},
		{name: "branch", msg: "fatal: couldn't find remote ref feature/nope", want: ErrBranchNotFound},
		{name: "remote", msg: "error: No such remote 'origin'", want: ErrRemoteMissing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyGitError(tc.msg); !errors.Is(got, tc.want) {
				t.Fatalf("classifyGitError() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRunClassifiesGitErrors(t *testing.T) {
	script := writeFakeGit(t, `#!/bin/sh
echo "fatal: couldn't find remote ref missing-branch" >&2
exit 128
`)
	r := Runner{Bin: script, Timeout: 5 * time.Second}
	_, err := r.Run(context.Background(), "", "fetch", "origin", "missing-branch")
	if !errors.Is(err, ErrBranchNotFound) {
		t.Fatalf("Run err = %v, want ErrBranchNotFound", err)
	}
}

func TestCloneRetriesTransientNetworkErrors(t *testing.T) {
	countPath := filepath.Join(t.TempDir(), "count")
	script := writeFakeGit(t, fmt.Sprintf(`#!/bin/sh
count=0
if [ -f %[1]q ]; then count=$(cat %[1]q); fi
count=$((count + 1))
echo "$count" > %[1]q
if [ "$count" -lt 3 ]; then
  echo "fatal: unable to access 'https://example.test/repo.git/': Could not resolve host: example.test" >&2
  exit 128
fi
exit 0
`, countPath))
	r := Runner{Bin: script, Timeout: 5 * time.Second, RetryAttempts: 3}
	if err := r.Clone(context.Background(), "https://example.test/org/repo.git", filepath.Join(t.TempDir(), "repo"), false); err != nil {
		t.Fatalf("Clone err = %v, want retry success", err)
	}
	raw, err := os.ReadFile(countPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(raw)) != "3" {
		t.Fatalf("retry count = %q, want 3", raw)
	}
}

func TestCloneDoesNotRetryAuthErrors(t *testing.T) {
	countPath := filepath.Join(t.TempDir(), "count")
	script := writeFakeGit(t, fmt.Sprintf(`#!/bin/sh
count=0
if [ -f %[1]q ]; then count=$(cat %[1]q); fi
count=$((count + 1))
echo "$count" > %[1]q
echo "git@example.test: Permission denied (publickey)." >&2
exit 128
`, countPath))
	r := Runner{Bin: script, Timeout: 5 * time.Second, RetryAttempts: 3}
	err := r.Clone(context.Background(), "https://example.test/org/repo.git", filepath.Join(t.TempDir(), "repo"), false)
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("Clone err = %v, want ErrAuth", err)
	}
	raw, err := os.ReadFile(countPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(raw)) != "1" {
		t.Fatalf("retry count = %q, want no retry", raw)
	}
}

// TestCloneRetryCleansPartialDestination (GIT-02): a transient mid-clone
// network failure leaves the destination partially populated. git does not
// remove a directory it did not create (dest is a pre-existing MkdirTemp dir),
// so a naive retry of the same argv would fail with "destination path already
// exists and is not empty". Clone must reset dest to a clean, empty directory
// before retrying so the second attempt succeeds.
func TestCloneRetryCleansPartialDestination(t *testing.T) {
	countPath := filepath.Join(t.TempDir(), "count")
	dest := filepath.Join(t.TempDir(), "repo")
	// Pre-create dest as hydrate does (os.MkdirTemp), so git treats it as
	// pre-existing and would NOT clean it on failure.
	if err := os.MkdirAll(dest, 0o750); err != nil {
		t.Fatal(err)
	}
	script := writeFakeGit(t, fmt.Sprintf(`#!/bin/sh
count=0
if [ -f %[1]q ]; then count=$(cat %[1]q); fi
count=$((count + 1))
echo "$count" > %[1]q
dest=""
for a in "$@"; do dest="$a"; done
if [ "$count" -lt 2 ]; then
  # Simulate a mid-clone network failure: leave dest partially populated.
  mkdir -p "$dest"
  echo "partial" > "$dest/partial-file"
  echo "fatal: the remote end hung up unexpectedly" >&2
  exit 128
fi
# Second attempt succeeds into a clean dest.
mkdir -p "$dest/.git"
exit 0
`, countPath))
	r := Runner{Bin: script, Timeout: 5 * time.Second, RetryAttempts: 2, RetryBackoff: time.Millisecond}
	if err := r.Clone(context.Background(), "https://example.test/org/repo.git", dest, true); err != nil {
		t.Fatalf("Clone err = %v, want retry success into clean dest", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "partial-file")); err == nil {
		t.Fatalf("partial file from failed attempt remains in dest; retry did not clean it (GIT-02)")
	}
	if _, err := os.Stat(filepath.Join(dest, ".git")); err != nil {
		t.Fatalf("successful retry did not populate dest: %v", err)
	}
	raw, err := os.ReadFile(countPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(raw)) != "2" {
		t.Fatalf("retry count = %q, want 2", raw)
	}
}

func TestUsesLFSDetectsGitAttributes(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".gitattributes"), []byte("# no-op\n*.bin filter=lfs diff=lfs merge=lfs -text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := UsesLFS(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Fatal("UsesLFS = false, want true")
	}
}

func TestUsesLFSIgnoresCommentsAndGitDir(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", ".gitattributes"), []byte("*.bin filter=lfs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitattributes"), []byte("# *.bin filter=lfs\n*.txt text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := UsesLFS(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Fatal("UsesLFS = true, want false")
	}
}

func TestRunTimesOutAndReportsNetworkError(t *testing.T) {
	// `exec sleep` so the shell is replaced by sleep (no grandchild holding the
	// output pipe), letting the context-kill return promptly.
	script := writeFakeGit(t, "#!/bin/sh\nexec sleep 5\n")
	r := Runner{Bin: script, Timeout: 100 * time.Millisecond}
	start := time.Now()
	_, err := r.Run(context.Background(), "", "fetch", "origin")
	elapsed := time.Since(start)
	if !errors.Is(err, ErrNetwork) {
		t.Fatalf("Run err = %v, want ErrNetwork on timeout", err)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("Run err = %v, want timeout message", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("Run took %s, want it to return near the 100ms timeout", elapsed)
	}
}

func TestResolveDefaultBranchRepairsAndReadsOriginHead(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "set-head-done")
	script := writeFakeGit(t, fmt.Sprintf(`#!/bin/sh
sub=""
while [ $# -gt 0 ]; do
  case "$1" in
    -c) shift 2;;
    *) sub="$1"; shift; break;;
  esac
done
case "$sub" in
  symbolic-ref)
    if [ -f %[1]q ]; then echo "origin/develop"; exit 0; fi
    echo "fatal: ref refs/remotes/origin/HEAD is not a symbolic ref" >&2; exit 128;;
  remote)
    : > %[1]q; exit 0;;
  *) exit 0;;
esac
`, statePath))
	r := Runner{Bin: script, Timeout: 5 * time.Second}
	branch, source, err := r.ResolveDefaultBranch(context.Background(), "", "main")
	if err != nil {
		t.Fatalf("ResolveDefaultBranch err = %v", err)
	}
	if branch != "develop" {
		t.Fatalf("branch = %q, want develop (from repaired origin/HEAD)", branch)
	}
	if source != DefaultBranchRemote {
		t.Fatalf("source = %q, want remote", source)
	}
}

func TestResolveDefaultBranchFallsBackToStored(t *testing.T) {
	script := writeFakeGit(t, `#!/bin/sh
sub=""
while [ $# -gt 0 ]; do
  case "$1" in
    -c) shift 2;;
    *) sub="$1"; shift; break;;
  esac
done
case "$sub" in
  symbolic-ref) echo "fatal: no origin HEAD" >&2; exit 128;;
  remote) exit 1;;
  rev-parse) exit 0;;
  *) exit 0;;
esac
`)
	r := Runner{Bin: script, Timeout: 5 * time.Second}
	branch, source, err := r.ResolveDefaultBranch(context.Background(), "", "trunk")
	if err != nil {
		t.Fatalf("ResolveDefaultBranch err = %v", err)
	}
	if branch != "trunk" || source != DefaultBranchStored {
		t.Fatalf("got (%q,%q), want (trunk, stored)", branch, source)
	}
}

func TestRemoteDefaultBranchParsesSymref(t *testing.T) {
	script := writeFakeGit(t, "#!/bin/sh\nprintf 'ref: refs/heads/develop\\tHEAD\\nabc123\\tHEAD\\n'\nexit 0\n")
	r := Runner{Bin: script, Timeout: 5 * time.Second}
	branch, err := r.RemoteDefaultBranch(context.Background(), "", "origin")
	if err != nil {
		t.Fatalf("RemoteDefaultBranch err = %v", err)
	}
	if branch != "develop" {
		t.Fatalf("branch = %q, want develop", branch)
	}
}

func TestDirtyStateClassifiesPorcelainOutput(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   DirtyState
	}{
		{"clean", "# branch.ab +0 -0\n", DirtyClean},
		{"dirty", "# branch.ab +0 -0\n1 .M N... 100644 100644 100644 aaa bbb file.go\n", DirtyDirty},
		{"untracked", "# branch.ab +0 -0\n? new.go\n", DirtyDirty},
		{"ahead", "# branch.ab +2 -0\n", DirtyAhead},
		{"behind", "# branch.ab +0 -3\n", DirtyBehind},
		{"diverged", "# branch.ab +1 -1\n", DirtyDiverged},
		{"conflicted", "# branch.ab +0 -0\nu UU N... file.go\n", DirtyConflicted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := writeFakeGit(t, "#!/bin/sh\ncat <<'EOF'\n"+tc.output+"EOF\n")
			r := Runner{Bin: script, Timeout: 5 * time.Second}
			got, err := r.DirtyState(context.Background(), "")
			if err != nil {
				t.Fatalf("DirtyState err = %v", err)
			}
			if got != tc.want {
				t.Fatalf("DirtyState = %q, want %q", got, tc.want)
			}
		})
	}
}

func writeFakeGit(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
