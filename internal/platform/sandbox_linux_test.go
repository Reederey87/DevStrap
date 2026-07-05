//go:build linux

package platform

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestBubblewrapCommandRejectsEmptyArgv(t *testing.T) {
	_, cleanup, err := BubblewrapSandbox{}.Command(context.Background(), SandboxSpec{}, nil)
	if err == nil || !strings.Contains(err.Error(), "empty argv") {
		t.Fatalf("Command() err = %v, want empty argv guard", err)
	}
	cleanup()
}

func TestBubblewrapCommandRejectsDashArgv0(t *testing.T) {
	_, cleanup, err := BubblewrapSandbox{}.Command(context.Background(), SandboxSpec{}, []string{"-bad"})
	if err == nil || !strings.Contains(err.Error(), "argv[0]") {
		t.Fatalf("Command() err = %v, want dash argv[0] guard", err)
	}
	cleanup()
}

func TestBubblewrapCommandWrapsArgvAndCleanupIsSafe(t *testing.T) {
	sb := BubblewrapSandbox{}
	if err := sb.Available(); err != nil {
		t.Skipf("bwrap unavailable: %v", err)
	}
	root := t.TempDir()
	worktree := filepath.Join(root, "wt")
	logs := filepath.Join(root, "logs")
	tmpDir := filepath.Join(root, "tmp")
	for _, dir := range []string{worktree, logs, tmpDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	res, err := probeBwrap()
	if err != nil {
		t.Fatal(err)
	}
	wrapped, cleanup, err := sb.Command(context.Background(), SandboxSpec{
		WorktreeDir: worktree,
		TmpDir:      tmpDir,
		LogDir:      logs,
	}, []string{"/bin/true", "--flag"})
	if err != nil {
		t.Fatal(err)
	}
	if wrapped[0] != res.path || !strings.HasSuffix(wrapped[0], "bwrap") {
		t.Fatalf("wrapped[0] = %q, want probed bwrap path %q", wrapped[0], res.path)
	}
	if !slices.Equal(wrapped[len(wrapped)-2:], []string{"/bin/true", "--flag"}) {
		t.Fatalf("wrapped argv does not end with child argv: %v", wrapped)
	}
	for _, seq := range [][]string{
		{"--ro-bind", "/", "/"},
		{"--bind", worktree, worktree},
		{"--bind", tmpDir, tmpDir},
	} {
		if indexSequence(wrapped, seq...) == -1 {
			t.Fatalf("missing sequence %v in %v", seq, wrapped)
		}
	}
	if slices.Contains(wrapped, logs) {
		t.Fatalf("LogDir leaked into wrapped argv: %v", wrapped)
	}
	cleanup()
	cleanup()
}

// TestExistingRealPathsFailsClosed pins the CodeRabbit review fix: a credential
// mask must be dropped ONLY when its path genuinely does not exist. A symlink
// resolving to its real target is masked at the target; a path that exists but
// cannot be resolved (here: a symlink cycle, standing in for permission/loop/IO
// errors) must be KEPT — dropping it would leave the credential readable inside
// the sandbox.
func TestExistingRealPathsFailsClosed(t *testing.T) {
	root := t.TempDir()

	realTarget := filepath.Join(root, "real-ssh")
	if err := os.MkdirAll(realTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link-ssh")
	if err := os.Symlink(realTarget, link); err != nil {
		t.Fatal(err)
	}
	resolvedTarget, err := filepath.EvalSymlinks(realTarget)
	if err != nil {
		t.Fatal(err)
	}

	// A self-referential symlink: it exists (Lstat succeeds) but EvalSymlinks
	// fails with a non-not-exist error — the fail-closed path.
	loop := filepath.Join(root, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(root, "absent")

	got := existingRealPaths([]string{link, loop, missing})

	if !slices.Contains(got, resolvedTarget) {
		t.Fatalf("resolvable symlink not masked at its target: got %v, want %q", got, resolvedTarget)
	}
	if !slices.Contains(got, loop) {
		t.Fatalf("unresolvable-but-present path dropped (fail-OPEN): got %v, want the literal %q kept", got, loop)
	}
	if slices.Contains(got, missing) {
		t.Fatalf("genuinely absent path kept: got %v, want %q dropped", got, missing)
	}
}

// TestBubblewrapSandboxEnforcement proves the kernel actually blocks what the
// mount namespace denies — writes outside the worktree, reads of sensitive
// paths, and network when requested — while allowing confined work. Env-gated
// like the Seatbelt and MinIO conformance tests because it execs real
// processes under bubblewrap.
func TestBubblewrapSandboxEnforcement(t *testing.T) {
	if os.Getenv("DEVSTRAP_SANDBOX_E2E") != "1" {
		t.Skip("set DEVSTRAP_SANDBOX_E2E=1 to run the bubblewrap enforcement test")
	}
	sb := BubblewrapSandbox{}
	if err := sb.Available(); err != nil {
		t.Skipf("bwrap unavailable: %v", err)
	}
	touch, err := exec.LookPath("touch")
	if err != nil {
		t.Skipf("touch unavailable: %v", err)
	}
	cat, err := exec.LookPath("cat")
	if err != nil {
		t.Skipf("cat unavailable: %v", err)
	}

	root := t.TempDir()
	worktree := filepath.Join(root, "worktree")
	logs := filepath.Join(root, "logs")
	fakeHome := filepath.Join(root, "home")
	sshDir := filepath.Join(fakeHome, ".ssh")
	// A dedicated tmp allow-dir, NOT os.TempDir(): t.TempDir() itself lives
	// under os.TempDir(), so allowing the real temp root would make every
	// "outside" path in this fixture silently allowed.
	tmpAllow := filepath.Join(root, "tmp")
	for _, dir := range []string{worktree, logs, sshDir, tmpAllow} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(sshDir, "id_ed25519")
	if err := os.WriteFile(secret, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	readable := filepath.Join(fakeHome, "readme.txt")
	if err := os.WriteFile(readable, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.txt")

	spec := SandboxSpec{
		WorktreeDir:        worktree,
		TmpDir:             tmpAllow,
		LogDir:             logs,
		UserHome:           fakeHome,
		DenySensitiveReads: true,
	}

	run := func(spec SandboxSpec, argv ...string) error {
		t.Helper()
		wrapped, cleanup, err := sb.Command(context.Background(), spec, argv)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		cmd := exec.Command(wrapped[0], wrapped[1:]...) //nolint:gosec // test fixture argv
		cmd.Dir = worktree
		return cmd.Run()
	}

	if err := run(spec, touch, filepath.Join(worktree, "inside.txt")); err != nil {
		t.Fatalf("write inside worktree blocked: %v", err)
	}
	if _, err := os.Stat(filepath.Join(worktree, "inside.txt")); err != nil {
		t.Fatalf("inside file missing on host: %v", err)
	}
	if err := run(spec, touch, outside); err == nil {
		t.Fatal("write OUTSIDE worktree succeeded, want kernel denial")
	}
	if _, statErr := os.Stat(outside); statErr == nil {
		t.Fatal("outside file exists despite sandbox")
	}
	if err := run(spec, cat, secret); err == nil {
		t.Fatal("read of masked ~/.ssh key succeeded, want kernel denial")
	}
	if err := run(spec, cat, readable); err != nil {
		t.Fatalf("read of non-sensitive home file blocked: %v", err)
	}
	if err := run(spec, touch, filepath.Join(logs, "tamper.txt")); err == nil {
		t.Fatal("write into the log dir succeeded, want kernel denial")
	}

	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Logf("skipping network sub-assertion: bash unavailable: %v", err)
		return
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	connect := []string{bash, "-c", fmt.Sprintf("exec 3<>/dev/tcp/127.0.0.1/%d", port)}
	denyNet := spec
	denyNet.DenyNetwork = true
	if err := run(denyNet, connect...); err == nil {
		t.Fatal("network connect succeeded with DenyNetwork=true, want netns denial")
	}
	allowNet := spec
	allowNet.DenyNetwork = false
	if err := run(allowNet, connect...); err != nil {
		t.Fatalf("network connect failed with DenyNetwork=false: %v", err)
	}
}
