//go:build darwin

package platform

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestSeatbeltSandboxEnforcement proves the kernel actually blocks what the
// profile denies — writes outside the worktree and reads of sensitive paths —
// while allowing confined work. Env-gated like the MinIO conformance test:
// it execs real processes under /usr/bin/sandbox-exec.
func TestSeatbeltSandboxEnforcement(t *testing.T) {
	if os.Getenv("DEVSTRAP_SANDBOX_E2E") != "1" {
		t.Skip("set DEVSTRAP_SANDBOX_E2E=1 to run the Seatbelt enforcement test")
	}
	sb := SeatbeltSandbox{}
	if err := sb.Available(); err != nil {
		t.Skipf("sandbox-exec unavailable: %v", err)
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

	spec := SandboxSpec{
		WorktreeDir:        worktree,
		TmpDir:             tmpAllow,
		LogDir:             logs,
		UserHome:           fakeHome,
		DenySensitiveReads: true,
	}

	run := func(argv ...string) error {
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

	if err := run("/usr/bin/touch", filepath.Join(worktree, "inside.txt")); err != nil {
		t.Fatalf("write inside worktree blocked: %v", err)
	}
	if err := run("/usr/bin/touch", outside); err == nil {
		t.Fatal("write OUTSIDE worktree succeeded, want kernel denial")
	}
	if _, statErr := os.Stat(outside); statErr == nil {
		t.Fatal("outside file exists despite sandbox")
	}
	if err := run("/bin/cat", secret); err == nil {
		t.Fatal("read of denied ~/.ssh key succeeded, want kernel denial")
	}
	// The deny is targeted: a read elsewhere in the fake home still works.
	readable := filepath.Join(fakeHome, "readme.txt")
	if err := os.WriteFile(readable, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run("/bin/cat", readable); err != nil {
		t.Fatalf("read of non-sensitive home file blocked: %v", err)
	}
	// The log dir holds the profile and the parent-written log; the child
	// must not be able to write there.
	if err := run("/usr/bin/touch", filepath.Join(logs, "tamper.txt")); err == nil {
		t.Fatal("write into the log dir succeeded, want kernel denial")
	}
}

func TestSeatbeltCommandWrapsArgvAndCleansUpProfile(t *testing.T) {
	sb := SeatbeltSandbox{}
	if err := sb.Available(); err != nil {
		t.Skipf("sandbox-exec unavailable: %v", err)
	}
	root := t.TempDir()
	worktree := filepath.Join(root, "wt")
	logs := filepath.Join(root, "logs")
	for _, dir := range []string{worktree, logs} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	wrapped, cleanup, err := sb.Command(context.Background(), SandboxSpec{
		WorktreeDir: worktree,
		TmpDir:      os.TempDir(),
		LogDir:      logs,
	}, []string{"/usr/bin/true", "--flag"})
	if err != nil {
		t.Fatal(err)
	}
	if wrapped[0] != sandboxExecPath || wrapped[1] != "-f" || wrapped[3] != "/usr/bin/true" || wrapped[4] != "--flag" {
		t.Fatalf("wrapped argv = %v", wrapped)
	}
	profilePath := wrapped[2]
	info, err := os.Stat(profilePath)
	if err != nil {
		t.Fatalf("profile not written: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("profile mode = %v, want 0600", info.Mode().Perm())
	}
	cleanup()
	if _, err := os.Stat(profilePath); !os.IsNotExist(err) {
		t.Fatalf("profile still exists after cleanup: %v", err)
	}
}
