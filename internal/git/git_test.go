package git

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
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

func TestIsSquashMergedDetectsSquashMerge(t *testing.T) {
	repo, runner := initSquashMergeRepo(t)
	runRealGit(t, runner.Bin, repo, "checkout", "-b", "feature")
	writeAndCommit(t, runner.Bin, repo, "one.txt", "one\n", "feature one")
	writeAndCommit(t, runner.Bin, repo, "two.txt", "two\n", "feature two")
	runRealGit(t, runner.Bin, repo, "checkout", "main")
	runRealGit(t, runner.Bin, repo, "merge", "--squash", "feature")
	runRealGit(t, runner.Bin, repo, "commit", "-m", "squash feature")

	got, err := runner.IsSquashMerged(context.Background(), repo, "feature", "main")
	if err != nil {
		t.Fatalf("IsSquashMerged: %v", err)
	}
	if !got {
		t.Fatal("IsSquashMerged = false, want true")
	}
}

func TestIsSquashMergedDetectsRebaseMerge(t *testing.T) {
	repo, runner := initSquashMergeRepo(t)
	runRealGit(t, runner.Bin, repo, "checkout", "-b", "feature")
	writeAndCommit(t, runner.Bin, repo, "one.txt", "one\n", "feature one")
	writeAndCommit(t, runner.Bin, repo, "two.txt", "two\n", "feature two")
	runRealGit(t, runner.Bin, repo, "checkout", "main")
	writeAndCommit(t, runner.Bin, repo, "main.txt", "main\n", "advance main")
	runRealGit(t, runner.Bin, repo, "cherry-pick", "feature~1", "feature")

	got, err := runner.IsSquashMerged(context.Background(), repo, "feature", "main")
	if err != nil {
		t.Fatalf("IsSquashMerged: %v", err)
	}
	if !got {
		t.Fatal("IsSquashMerged = false, want true")
	}
}

func TestIsSquashMergedFalseForUnmerged(t *testing.T) {
	repo, runner := initSquashMergeRepo(t)
	runRealGit(t, runner.Bin, repo, "checkout", "-b", "feature")
	writeAndCommit(t, runner.Bin, repo, "one.txt", "one\n", "feature one")

	got, err := runner.IsSquashMerged(context.Background(), repo, "feature", "main")
	if err != nil {
		t.Fatalf("IsSquashMerged: %v", err)
	}
	if got {
		t.Fatal("IsSquashMerged = true, want false")
	}
}

func TestIsSquashMergedConservativeOnContentDivergence(t *testing.T) {
	repo, runner := initSquashMergeRepo(t)
	runRealGit(t, runner.Bin, repo, "checkout", "-b", "feature")
	writeAndCommit(t, runner.Bin, repo, "README.md", "feature\n", "feature readme")
	runRealGit(t, runner.Bin, repo, "checkout", "main")
	writeAndCommit(t, runner.Bin, repo, "README.md", "main\n", "main readme")

	got, err := runner.IsSquashMerged(context.Background(), repo, "feature", "main")
	if err != nil {
		t.Fatalf("IsSquashMerged: %v", err)
	}
	if got {
		t.Fatal("IsSquashMerged = true, want false")
	}
}

// TestIsSquashMergedFalseAfterRevertOnBase (dual-review fix): a change that
// was merged into base and then REVERTED must read as NOT merged — the branch
// still carries work absent from the CURRENT base tree. The earlier patch-id
// approach matched the historical (reverted) commit and would have reaped it.
func TestIsSquashMergedFalseAfterRevertOnBase(t *testing.T) {
	repo, runner := initSquashMergeRepo(t)
	runRealGit(t, runner.Bin, repo, "checkout", "-b", "feature")
	writeAndCommit(t, runner.Bin, repo, "one.txt", "one\n", "feature one")
	runRealGit(t, runner.Bin, repo, "checkout", "main")
	runRealGit(t, runner.Bin, repo, "merge", "--squash", "feature")
	runRealGit(t, runner.Bin, repo, "commit", "-m", "squash feature")
	runRealGit(t, runner.Bin, repo, "revert", "--no-edit", "HEAD")

	got, err := runner.IsSquashMerged(context.Background(), repo, "feature", "main")
	if err != nil {
		t.Fatalf("IsSquashMerged: %v", err)
	}
	if got {
		t.Fatal("IsSquashMerged = true, want false (squash was reverted on base)")
	}
}

// TestIsSquashMergedMatchesCoincidentallyIdenticalDiff pins the DOCUMENTED
// accepted limitation: a branch whose net change also landed via an unrelated
// identical commit is indistinguishable from a squash-merge by any content-
// equivalence test and reads as merged. If this test ever fails, the
// limitation was fixed — update spec/08 and the reap-breadcrumb rationale.
func TestIsSquashMergedMatchesCoincidentallyIdenticalDiff(t *testing.T) {
	repo, runner := initSquashMergeRepo(t)
	runRealGit(t, runner.Bin, repo, "checkout", "-b", "feature")
	writeAndCommit(t, runner.Bin, repo, "one.txt", "identical\n", "feature adds one.txt")
	runRealGit(t, runner.Bin, repo, "checkout", "main")
	writeAndCommit(t, runner.Bin, repo, "one.txt", "identical\n", "unrelated identical change")

	got, err := runner.IsSquashMerged(context.Background(), repo, "feature", "main")
	if err != nil {
		t.Fatalf("IsSquashMerged: %v", err)
	}
	if !got {
		t.Fatal("IsSquashMerged = false; the documented coincidental-identical-diff limitation appears fixed — update spec/08")
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

func TestRunTimesOutAndReportsTimeoutError(t *testing.T) {
	// `exec sleep` so the shell is replaced by sleep (no grandchild holding the
	// output pipe), letting the context-kill return promptly.
	script := writeFakeGit(t, "#!/bin/sh\nexec sleep 5\n")
	r := Runner{Bin: script, Timeout: 100 * time.Millisecond}
	start := time.Now()
	_, err := r.Run(context.Background(), "", "fetch", "origin")
	elapsed := time.Since(start)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Run err = %v, want ErrTimeout on timeout", err)
	}
	var cmdErr CommandError
	if !errors.As(err, &cmdErr) || !errors.Is(cmdErr.Kind, ErrTimeout) {
		t.Fatalf("Run err = %#v, want CommandError kind ErrTimeout", err)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("Run err = %v, want timeout message", err)
	}
	// A short-class timeout must not misdirect the user at a knob that only
	// governs the transfer class (P6-GIT-01 review).
	if strings.Contains(err.Error(), "clone_timeout") {
		t.Fatalf("Run err = %v, want NO clone_timeout hint on a non-transfer command", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("Run took %s, want it to return near the 100ms timeout", elapsed)
	}
}

func TestCommandErrorExposesSubprocessExitCode(t *testing.T) {
	script := writeFakeGit(t, "#!/bin/sh\nexit 1\n")
	r := Runner{Bin: script, Timeout: 5 * time.Second}
	_, err := r.Run(context.Background(), "", "merge-base", "--is-ancestor", "old", "new")
	var cmdErr CommandError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("Run err = %#v, want CommandError", err)
	}
	if got := cmdErr.ExitCode(); got != 1 {
		t.Fatalf("CommandError.ExitCode() = %d, want 1", got)
	}
}

func TestCloneTimeoutIsTerminalAndDoesNotRetryOrWipe(t *testing.T) {
	tmp := t.TempDir()
	countPath := filepath.Join(tmp, "count")
	dest := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(dest, 0o750); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(dest, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	// `exec sleep 5` (not 1): the 500ms deadline must win against the natural
	// exit even when the whole -race suite is loading the machine; a 2x margin
	// flaked, 10x holds. The killed child returns at the deadline, so the
	// longer sleep does not slow the test down.
	script := writeFakeGit(t, fmt.Sprintf(`#!/bin/sh
echo attempt >> %[1]q
exec sleep 5
`, countPath))
	r := Runner{Bin: script, Timeout: 5 * time.Second, LongTimeout: 500 * time.Millisecond, RetryAttempts: 3, RetryBackoff: time.Millisecond}
	err := r.CloneWithOptions(context.Background(), "https://example.test/org/repo.git", dest, CloneOptions{Partial: true})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("CloneWithOptions err = %v, want ErrTimeout", err)
	}
	var cmdErr CommandError
	if !errors.As(err, &cmdErr) || !errors.Is(cmdErr.Kind, ErrTimeout) {
		t.Fatalf("CloneWithOptions err = %#v, want CommandError kind ErrTimeout", err)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("CloneWithOptions err = %v, want timeout message", err)
	}
	// At most one attempt: the timeout is terminal, so the retry loop must not
	// fire. Zero is legal — under load the kill can land before the fake git
	// logs (see attemptCount).
	if got := attemptCount(t, countPath); got > 1 {
		t.Fatalf("attempt count = %d, want at most 1 (timeout must not retry)", got)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("destination was wiped after terminal timeout: %v", err)
	}
}

func TestCloneUsesLongTimeoutInsteadOfShortTimeout(t *testing.T) {
	countPath := filepath.Join(t.TempDir(), "count")
	script := writeFakeGit(t, fmt.Sprintf(`#!/bin/sh
echo attempt >> %[1]q
sleep 0.2
exit 0
`, countPath))
	// LongTimeout 30s (not 2s): the success path returns at ~0.2s; the wide
	// deadline only guards against a loaded machine stretching the child's
	// startup past the transfer deadline and flipping the test's premise.
	r := Runner{Bin: script, Timeout: 50 * time.Millisecond, LongTimeout: 30 * time.Second, RetryAttempts: 1}
	if err := r.CloneWithOptions(context.Background(), "https://example.test/org/repo.git", filepath.Join(t.TempDir(), "repo"), CloneOptions{Partial: true}); err != nil {
		t.Fatalf("CloneWithOptions err = %v, want success under LongTimeout", err)
	}
	if got := attemptCount(t, countPath); got != 1 {
		t.Fatalf("attempt count = %d, want 1", got)
	}
}

func TestFetchTimeoutIsTerminalAndDoesNotRetry(t *testing.T) {
	countPath := filepath.Join(t.TempDir(), "count")
	script := writeFakeGit(t, fmt.Sprintf(`#!/bin/sh
echo attempt >> %[1]q
exec sleep 5
`, countPath))
	r := Runner{Bin: script, Timeout: 5 * time.Second, LongTimeout: 500 * time.Millisecond, RetryAttempts: 3, RetryBackoff: time.Millisecond}
	err := r.Fetch(context.Background(), "", "origin", "main")
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Fetch err = %v, want ErrTimeout", err)
	}
	var cmdErr CommandError
	if !errors.As(err, &cmdErr) || !errors.Is(cmdErr.Kind, ErrTimeout) {
		t.Fatalf("Fetch err = %#v, want CommandError kind ErrTimeout", err)
	}
	if got := attemptCount(t, countPath); got > 1 {
		t.Fatalf("attempt count = %d, want at most 1 (timeout must not retry)", got)
	}
}

func TestLFSPullTimeoutIsTerminalAndDoesNotRetry(t *testing.T) {
	countPath := filepath.Join(t.TempDir(), "count")
	script := writeFakeGit(t, fmt.Sprintf(`#!/bin/sh
echo attempt >> %[1]q
exec sleep 5
`, countPath))
	r := Runner{Bin: script, Timeout: 5 * time.Second, LongTimeout: 500 * time.Millisecond, RetryAttempts: 3, RetryBackoff: time.Millisecond}
	err := r.LFSPull(context.Background(), "")
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("LFSPull err = %v, want ErrTimeout", err)
	}
	var cmdErr CommandError
	if !errors.As(err, &cmdErr) || !errors.Is(cmdErr.Kind, ErrTimeout) {
		t.Fatalf("LFSPull err = %#v, want CommandError kind ErrTimeout", err)
	}
	if got := attemptCount(t, countPath); got > 1 {
		t.Fatalf("attempt count = %d, want at most 1 (timeout must not retry)", got)
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

func TestLocalDefaultBranchReadsLocalSymbolicRefWithoutNetwork(t *testing.T) {
	repo, r := initLocalDefaultBranchRepo(t)
	runRealGit(t, r.Bin, repo, "update-ref", "refs/remotes/origin/trunk", "HEAD")
	runRealGit(t, r.Bin, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/trunk")

	start := time.Now()
	branch, source, err := r.LocalDefaultBranch(context.Background(), repo, "main")
	if err != nil {
		t.Fatalf("LocalDefaultBranch err = %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("LocalDefaultBranch took %s, want local-only prompt return", elapsed)
	}
	if branch != "trunk" || source != DefaultBranchRemote {
		t.Fatalf("got (%q,%q), want (trunk, remote)", branch, source)
	}
}

func TestLocalDefaultBranchFallsBackToStoredRefOffline(t *testing.T) {
	repo, r := initLocalDefaultBranchRepo(t)
	runRealGit(t, r.Bin, repo, "update-ref", "refs/remotes/origin/main", "HEAD")

	branch, source, err := r.LocalDefaultBranch(context.Background(), repo, "main")
	if err != nil {
		t.Fatalf("LocalDefaultBranch err = %v", err)
	}
	if branch != "main" || source != DefaultBranchStored {
		t.Fatalf("got (%q,%q), want (main, stored)", branch, source)
	}
}

func TestLocalDefaultBranchErrorsWhenNoLocalRefOffline(t *testing.T) {
	repo, r := initLocalDefaultBranchRepo(t)

	branch, source, err := r.LocalDefaultBranch(context.Background(), repo, "main")
	if err == nil {
		t.Fatal("LocalDefaultBranch succeeded, want offline error")
	}
	if branch != "" || source != "" {
		t.Fatalf("got (%q,%q), want empty branch and source", branch, source)
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

// attemptCount reports how many times the fake git logged an attempt. A
// missing file counts as zero: under full-suite -race load the transfer
// deadline can kill the child before its first `echo attempt` executes, so
// timeout tests must treat "never got to log" as "no retry happened", not as
// a test failure.
func attemptCount(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(strings.Fields(string(raw)))
}

// QUAL-06: jitterDelay produces full-jitter capped-exponential backoff. Delays
// must stay within [1, min(cap, base*2^(attempt-1))] and the cap must clamp
// exponential growth.
func TestJitterDelayFullJitterBounded(t *testing.T) {
	base := 200 * time.Millisecond
	cap := 5 * time.Second
	rng := rand.New(rand.NewSource(42)) // deterministic
	randFn := rng.Int63n

	for attempt := 1; attempt <= 6; attempt++ {
		d := jitterDelay(base, cap, attempt, randFn)
		upper := int64(cap)
		if exp := int64(base) * (int64(1) << uint(attempt-1)); exp < upper {
			upper = exp
		}
		if d < 1 || d > time.Duration(upper) {
			t.Fatalf("attempt %d: delay %s outside [1, %s]", attempt, d, time.Duration(upper))
		}
	}
	// Once base*2^n exceeds cap, the delay is clamped to [1, cap].
	big := jitterDelay(base, cap, 30, randFn)
	if big > cap {
		t.Fatalf("delay %s exceeds cap %s", big, cap)
	}
	// Zero/negative base short-circuits (no delay).
	if d := jitterDelay(0, cap, 1, randFn); d != 0 {
		t.Fatalf("zero base delay = %s, want 0", d)
	}
}

func TestCloneArgsSubmodules(t *testing.T) {
	cases := []struct {
		name string
		opts CloneOptions
		want []string
	}{
		{name: "plain", opts: CloneOptions{}, want: []string{"clone", "--", "r", "d"}},
		{name: "partial", opts: CloneOptions{Partial: true}, want: []string{"clone", "--filter=blob:none", "--", "r", "d"}},
		{name: "submodules", opts: CloneOptions{Submodules: true}, want: []string{"clone", "--recurse-submodules", "--", "r", "d"}},
		{name: "partial+submodules", opts: CloneOptions{Partial: true, Submodules: true, AlsoFilterSubmodules: true}, want: []string{"clone", "--filter=blob:none", "--also-filter-submodules", "--recurse-submodules", "--", "r", "d"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := cloneArgs("r", "d", c.opts)
			if !slices.Equal(got, c.want) {
				t.Fatalf("cloneArgs = %#v, want %#v", got, c.want)
			}
		})
	}
}

// TestCloneWithOptionsInitializesSubmodules (GIT-06) clones a superproject
// that has a submodule with --recurse-submodules and verifies the submodule
// working tree is present on disk. Uses the real git binary against local
// file-path remotes.
func TestCloneWithOptionsInitializesSubmodules(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	tmp := t.TempDir()
	gitEnv := []string{
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_NOSYSTEM=true",
		"HOME=" + tmp,
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=protocol.file.allow", "GIT_CONFIG_VALUE_0=always",
	}
	run := func(dir string, args ...string) error {
		c := exec.Command(gitBin, args...)
		c.Dir = dir
		c.Env = append(os.Environ(), gitEnv...)
		out, err := c.CombinedOutput()
		if err != nil {
			return fmt.Errorf("git %v: %w\n%s", args, err, out)
		}
		return nil
	}
	// Submodule remote.
	sub := filepath.Join(tmp, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := run(sub, "init"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "README.md"), []byte("sub\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(sub, "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	if err := run(sub, "commit", "-m", "sub init"); err != nil {
		t.Fatal(err)
	}
	// Superproject remote.
	main := filepath.Join(tmp, "main")
	if err := os.MkdirAll(main, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := run(main, "init"); err != nil {
		t.Fatal(err)
	}
	if err := run(main, "submodule", "add", sub, "vendor/sub"); err != nil {
		t.Fatal(err)
	}
	if err := run(main, "commit", "-m", "add submodule"); err != nil {
		t.Fatal(err)
	}
	// Clone with submodules.
	dest := filepath.Join(tmp, "dest")
	r := Runner{Bin: gitBin, Timeout: 30 * time.Second}
	if err := r.CloneWithOptions(context.Background(), main, dest, CloneOptions{Submodules: true}); err != nil {
		t.Fatalf("CloneWithOptions: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "vendor", "sub", "README.md")); err != nil {
		t.Fatalf("submodule not materialized: %v", err)
	}
}

func initLocalDefaultBranchRepo(t *testing.T) (string, Runner) {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runRealGit(t, gitBin, repo, "init")
	runRealGit(t, gitBin, repo, "config", "user.name", "t")
	runRealGit(t, gitBin, repo, "config", "user.email", "t@example.com")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runRealGit(t, gitBin, repo, "add", "README.md")
	runRealGit(t, gitBin, repo, "commit", "-m", "init")
	// Blackhole remote (RFC 5737 TEST-NET-1, non-routable): any accidental
	// network call (a reintroduced set-head --auto / ls-remote) HANGS until the
	// runner Timeout rather than failing fast, so it trips the sub-second
	// elapsed budget the no-network tests assert. The correct local-only path
	// (symbolic-ref / rev-parse) returns in milliseconds.
	runRealGit(t, gitBin, repo, "remote", "add", "origin", "https://192.0.2.1/none.git")
	return repo, Runner{Bin: gitBin, Timeout: 5 * time.Second}
}

func initSquashMergeRepo(t *testing.T) (string, Runner) {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runRealGit(t, gitBin, repo, "init")
	runRealGit(t, gitBin, repo, "config", "user.name", "t")
	runRealGit(t, gitBin, repo, "config", "user.email", "t@example.com")
	runRealGit(t, gitBin, repo, "checkout", "-b", "main")
	writeAndCommit(t, gitBin, repo, "README.md", "base\n", "init")
	return repo, Runner{Bin: gitBin, Timeout: 5 * time.Second}
}

func writeAndCommit(t *testing.T, gitBin, repo, name, contents, message string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	runRealGit(t, gitBin, repo, "add", name)
	runRealGit(t, gitBin, repo, "commit", "-m", message)
}

func runRealGit(t *testing.T, gitBin, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(gitBin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=true",
		"GIT_TERMINAL_PROMPT=0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// P6-GIT-01 review: push is the same transfer class as clone — it gets the
// long deadline, a terminal timeout, and the config hint.
func TestPushBranchTimeoutIsTerminalWithHint(t *testing.T) {
	countPath := filepath.Join(t.TempDir(), "count")
	script := writeFakeGit(t, fmt.Sprintf(`#!/bin/sh
echo attempt >> %[1]q
exec sleep 5
`, countPath))
	r := Runner{Bin: script, Timeout: 5 * time.Second, LongTimeout: 500 * time.Millisecond}
	err := r.PushBranch(context.Background(), "", "origin", "agent/x")
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("PushBranch err = %v, want ErrTimeout", err)
	}
	if !strings.Contains(err.Error(), "clone_timeout") {
		t.Fatalf("PushBranch err = %v, want the clone_timeout hint on a transfer-class timeout", err)
	}
	if got := attemptCount(t, countPath); got > 1 {
		t.Fatalf("attempt count = %d, want at most 1 (timeout must not retry)", got)
	}
}

// P6-GIT-01 review: LongTimeout <= 0 means the transfer class is explicitly
// unbounded — it must NOT silently fall back to the short 2m-class cap.
func TestZeroLongTimeoutMeansUnboundedTransfer(t *testing.T) {
	script := writeFakeGit(t, "#!/bin/sh\nsleep 0.2\nexit 0\n")
	r := Runner{Bin: script, Timeout: 50 * time.Millisecond, LongTimeout: 0, RetryAttempts: 1}
	if err := r.CloneWithOptions(context.Background(), "https://example.test/org/repo.git", filepath.Join(t.TempDir(), "repo"), CloneOptions{Partial: true}); err != nil {
		t.Fatalf("CloneWithOptions err = %v, want success with unbounded transfer class", err)
	}
}

// TestStashCreate: a clean tree yields ok=false and no error; a dirty tree
// yields a non-empty commit sha, and — the property that distinguishes
// `git stash create` from `git stash push` — leaves the worktree and index
// completely untouched.
func TestStashCreate(t *testing.T) {
	repo, r := initSquashMergeRepo(t)
	ctx := context.Background()

	if sha, ok, err := r.StashCreate(ctx, repo); err != nil {
		t.Fatalf("StashCreate on a clean tree err = %v", err)
	} else if ok {
		t.Fatalf("StashCreate on a clean tree = (%q, true), want ok=false", sha)
	}

	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sha, ok, err := r.StashCreate(ctx, repo)
	if err != nil {
		t.Fatalf("StashCreate on a dirty tree err = %v", err)
	}
	if !ok || sha == "" {
		t.Fatalf("StashCreate on a dirty tree = (%q, %v), want a non-empty sha and ok=true", sha, ok)
	}

	dirty, err := r.DirtyState(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if dirty != DirtyDirty {
		t.Fatalf("DirtyState after StashCreate = %q, want the working tree to remain dirty (untouched)", dirty)
	}
	content, err := os.ReadFile(filepath.Join(repo, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "dirty\n" {
		t.Fatalf("README.md = %q after StashCreate, want the working tree left untouched", content)
	}
}

// TestPushRef pushes a StashCreate commit straight to a WIP ref on a bare
// remote via a raw refspec (Layer B, spec/07), then verifies the ref lands at
// the pushed sha on the remote without ever creating or touching a local
// branch.
func TestPushRef(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	runRealGit(t, gitBin, tmp, "init", "--bare", remote)

	repo, r := initSquashMergeRepo(t)
	runRealGit(t, gitBin, repo, "remote", "add", "origin", remote)

	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sha, ok, err := r.StashCreate(ctx, repo)
	if err != nil {
		t.Fatalf("StashCreate err = %v", err)
	}
	if !ok {
		t.Fatal("StashCreate returned ok=false on a dirty tree")
	}

	ref := "refs/devstrap/wip/dev_test123/work/acme/api-server"
	if err := r.PushRef(ctx, repo, "origin", sha, ref); err != nil {
		t.Fatalf("PushRef err = %v", err)
	}

	got, err := r.Run(ctx, remote, "show-ref", "--hash", ref)
	if err != nil {
		t.Fatalf("show-ref on remote: %v", err)
	}
	if strings.TrimSpace(got) != sha {
		t.Fatalf("remote ref %s = %q, want the pushed sha %q", ref, got, sha)
	}

	branches, err := r.Run(ctx, repo, "branch", "--list")
	if err != nil {
		t.Fatalf("branch --list: %v", err)
	}
	if strings.Contains(branches, "wip") {
		t.Fatalf("PushRef must not create a local branch: %q", branches)
	}
}

// TestPushRefRejectsRefOutsideWipNamespace pins that PushRef refuses to push
// to a ref outside refs/devstrap/wip/*, never falling through to actually
// invoke git — the caller-supplied ref for this primitive must never be able
// to land on, say, refs/heads/main.
func TestPushRefRejectsRefOutsideWipNamespace(t *testing.T) {
	r := Runner{Bin: "git", Timeout: 5 * time.Second}
	if err := r.PushRef(context.Background(), "", "origin", "deadbeef", "refs/heads/main"); err == nil {
		t.Fatal("PushRef succeeded pushing to refs/heads/main, want a rejected ref error")
	}
}

func TestSafeRefPath(t *testing.T) {
	valid := []string{
		"refs/devstrap/wip/dev_01912e3d/work/acme/api-server",
		"refs/devstrap/wip/dev_x/path",
	}
	for _, ref := range valid {
		if !safeRefPath(ref) {
			t.Errorf("safeRefPath(%q) = false, want true", ref)
		}
	}
	invalid := []string{
		"refs/heads/main",
		"refs/devstrap/wip/dev_x", // missing path segment
		"refs/devstrap/wip/../escape/path",
		"refs/devstrap/wip/-dev/path",
		"refs/devstrap/wip/dev x/path", // whitespace
		"",
	}
	for _, ref := range invalid {
		if safeRefPath(ref) {
			t.Errorf("safeRefPath(%q) = true, want false", ref)
		}
	}
}
