//go:build linux

package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// TestMain doubles as the sandbox-helper re-exec target: LandlockSandbox
// wraps argv with os.Executable(), which under `go test` is THIS test binary
// — it has no cobra, so the shim dispatch is replicated here (crash-test
// pattern). A second intercept exposes raw truncate(2) so the e2e can pin the
// V3 handled set: coreutils truncate open()s first, which V1 write-denial
// already blocks, and would prove nothing about the TRUNCATE right.
func TestMain(m *testing.M) {
	if len(os.Args) > 3 && os.Args[1] == SandboxHelperCommand && os.Args[2] == "--spec" {
		var spec SandboxSpec
		if err := json.Unmarshal([]byte(os.Args[3]), &spec); err != nil {
			fmt.Fprintf(os.Stderr, "test sandbox-helper shim: parse spec: %v\n", err)
			os.Exit(125)
		}
		argv := os.Args[4:]
		if len(argv) > 0 && argv[0] == "--" {
			argv = argv[1:]
		}
		if err := ExecSandboxHelper(spec, argv); err != nil {
			fmt.Fprintf(os.Stderr, "test sandbox-helper shim: %v\n", err)
		}
		os.Exit(125)
	}
	if len(os.Args) == 3 && os.Args[1] == "landlock-truncate-probe" {
		if err := syscall.Truncate(os.Args[2], 0); err != nil {
			fmt.Fprintf(os.Stderr, "truncate: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestLandlockSandboxEnforcement proves the kernel actually enforces the
// fallback's write confinement through the re-exec shim — and, just as
// deliberately, pins what it does NOT enforce: credential reads stay allowed
// (spec/18 decision — additive-allow, read-denial is bubblewrap-only). A
// future slice that adds read-denial must consciously update this test and
// landlockLimitations together. Env-gated like the Seatbelt/bubblewrap
// enforcement tests because it execs real processes under restriction.
func TestLandlockSandboxEnforcement(t *testing.T) {
	if os.Getenv("DEVSTRAP_SANDBOX_E2E") != "1" {
		t.Skip("set DEVSTRAP_SANDBOX_E2E=1 to run the landlock enforcement test")
	}
	sb := LandlockSandbox{}
	if err := sb.Available(); err != nil {
		t.Skipf("landlock unavailable: %v", err)
	}
	touch, err := exec.LookPath("touch")
	if err != nil {
		t.Skipf("touch unavailable: %v", err)
	}
	cat, err := exec.LookPath("cat")
	if err != nil {
		t.Skipf("cat unavailable: %v", err)
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh unavailable: %v", err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
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
	outside := filepath.Join(root, "outside.txt")
	outsideExisting := filepath.Join(root, "existing.txt")
	if err := os.WriteFile(outsideExisting, []byte("shrink me"), 0o644); err != nil {
		t.Fatal(err)
	}
	insideExisting := filepath.Join(worktree, "inside-existing.txt")
	if err := os.WriteFile(insideExisting, []byte("shrink me"), 0o644); err != nil {
		t.Fatal(err)
	}

	spec := SandboxSpec{
		WorktreeDir:        worktree,
		TmpDir:             tmpAllow,
		LogDir:             logs,
		UserHome:           fakeHome,
		DenySensitiveReads: true, // deliberately set — and deliberately NOT honored by this backend
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
	if err := run(spec, touch, filepath.Join(tmpAllow, "scratch.txt")); err != nil {
		t.Fatalf("write inside tmp allow-dir blocked: %v", err)
	}
	if err := run(spec, touch, outside); err == nil {
		t.Fatal("write OUTSIDE worktree succeeded, want kernel denial")
	}
	if _, statErr := os.Stat(outside); statErr == nil {
		t.Fatal("outside file exists despite sandbox")
	}
	// Raw truncate(2) of an outside file pins the V3 floor: under a V2-only
	// handled set this would SUCCEED on every kernel even though open-for-write
	// is denied — the exact write-confinement bypass landlockMinABI=3 closes.
	if err := run(spec, self, "landlock-truncate-probe", outsideExisting); err == nil {
		t.Fatal("truncate(2) of outside file succeeded, want kernel denial (V3 TRUNCATE)")
	}
	if data, err := os.ReadFile(outsideExisting); err != nil || len(data) == 0 {
		t.Fatalf("outside file was truncated despite sandbox: %v (len %d)", err, len(data))
	}
	if err := run(spec, self, "landlock-truncate-probe", insideExisting); err != nil {
		t.Fatalf("truncate(2) inside worktree blocked: %v", err)
	}
	if err := run(spec, sh, "-c", "echo x > /dev/null"); err != nil {
		t.Fatalf("write to /dev/null blocked (device-node grant missing): %v", err)
	}
	if err := run(spec, touch, filepath.Join(logs, "tamper.txt")); err == nil {
		t.Fatal("write into the log dir succeeded, want kernel denial")
	}
	// The documented degrade, pinned on purpose: this backend is
	// additive-allow, so DenySensitiveReads is NOT enforceable — the
	// credential stays readable and resolveAgentSandbox surfaces that through
	// SandboxCapabilities.Limitations (spec/18, PR #121 follow-up decision).
	if err := run(spec, cat, secret); err != nil {
		t.Fatalf("credential read failed under landlock — the documented additive-allow degrade no longer holds; update landlockLimitations and spec/15 together: %v", err)
	}
	// Exit-code fidelity through re-exec: the shim execve()s in the same PID,
	// so the agent's own exit code must come back untouched.
	err = run(spec, sh, "-c", "exit 7")
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 7 {
		t.Fatalf("exit-code fidelity broken through the shim: err = %v, want ExitError 7", err)
	}

	abi, err := probeLandlock()
	if err != nil {
		t.Fatal(err)
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
	if abi >= 4 {
		denyNet := spec
		denyNet.DenyNetwork = true
		if err := run(denyNet, connect...); err == nil {
			t.Fatal("TCP connect succeeded with DenyNetwork=true on ABI >= 4, want denial")
		}
	} else {
		t.Logf("kernel landlock ABI %d < 4: TCP deny sub-assertion skipped (the documented degrade)", abi)
	}
	allowNet := spec
	allowNet.DenyNetwork = false
	if err := run(allowNet, connect...); err != nil {
		t.Fatalf("TCP connect failed with DenyNetwork=false: %v", err)
	}
}
