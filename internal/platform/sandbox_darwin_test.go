//go:build darwin

package platform

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	// ~/.ssh is a SYMLINK to an out-of-worktree real dir holding the fake key.
	// Seatbelt matches the kernel-real path, so the deny must resolve the leaf
	// symlink or the key stays readable — P4-GIT-03 leaf-symlink parity with
	// bubblewrap. realSSH sits under root but outside every allow path
	// (worktree/tmpAllow/logs), so before the fix it would be readable.
	realSSH := filepath.Join(root, "real-ssh")
	sshDir := filepath.Join(fakeHome, ".ssh")
	// A dedicated tmp allow-dir, NOT os.TempDir(): t.TempDir() itself lives
	// under os.TempDir(), so allowing the real temp root would make every
	// "outside" path in this fixture silently allowed.
	tmpAllow := filepath.Join(root, "tmp")
	for _, dir := range []string{worktree, logs, fakeHome, realSSH, tmpAllow} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(realSSH, sshDir); err != nil {
		t.Fatal(err)
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
		sc, err := sb.Command(context.Background(), spec, argv)
		if err != nil {
			t.Fatal(err)
		}
		defer sc.Cleanup()
		cmd := exec.Command(sc.Argv[0], sc.Argv[1:]...) //nolint:gosec // test fixture argv
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
	// Read through the ~/.ssh SYMLINK: the kernel resolves it to realSSH, so
	// this is denied only because the profile denies the RESOLVED leaf target.
	if err := run("/bin/cat", secret); err == nil {
		t.Fatal("read of denied ~/.ssh key (via symlink) succeeded, want kernel denial")
	}
	// Read via the resolved real path directly — before the leaf-symlink fix
	// the literal ~/.ssh deny never covered this and the key was readable.
	if err := run("/bin/cat", filepath.Join(realSSH, "id_ed25519")); err == nil {
		t.Fatal("read of the resolved ~/.ssh target succeeded, want kernel denial")
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

// TestSeatbeltResolvesCredentialLeafSymlinks pins the P4-GIT-03 leaf-symlink
// parity fix: when a credential dir is itself a symlink (~/.ssh -> realssh),
// the emitted profile must deny BOTH the literal alias AND the resolved target,
// because Seatbelt matches the kernel-real path. Absent cred dirs still get
// their literal deny (never dropped).
func TestSeatbeltResolvesCredentialLeafSymlinks(t *testing.T) {
	root := t.TempDir()
	fakeHome := filepath.Join(root, "home")
	worktree := filepath.Join(root, "wt")
	logs := filepath.Join(root, "logs")
	realSSH := filepath.Join(fakeHome, "realssh")
	for _, dir := range []string{fakeHome, worktree, logs, realSSH} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// ~/.ssh is a symlink to a real dir; ~/.aws is deliberately absent.
	sshLink := filepath.Join(fakeHome, ".ssh")
	if err := os.Symlink(realSSH, sshLink); err != nil {
		t.Fatal(err)
	}

	sc, err := SeatbeltSandbox{}.Command(context.Background(), SandboxSpec{
		WorktreeDir:        worktree,
		TmpDir:             root, // any existing, resolvable dir
		LogDir:             logs,
		UserHome:           fakeHome,
		DenySensitiveReads: true,
	}, []string{"/usr/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Cleanup()
	wrapped := sc.Argv

	// wrapped = [sandbox-exec -f <profile> /usr/bin/true]; the profile path is
	// wrapped[2].
	data, err := os.ReadFile(wrapped[2])
	if err != nil {
		t.Fatalf("read profile: %v", err)
	}
	profile := string(data)

	// resolveSandboxSpecPaths resolves UserHome first, so the anchors are
	// anchored under the real home.
	realHome, err := filepath.EvalSymlinks(fakeHome)
	if err != nil {
		t.Fatal(err)
	}
	resolvedSSH, err := filepath.EvalSymlinks(realSSH)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		// Literal alias — denied even though it is a symlink.
		`(subpath ` + sbplQuote(filepath.Join(realHome, ".ssh")) + `)`,
		// Resolved target — the deny that actually fires in the kernel.
		`(subpath ` + sbplQuote(resolvedSSH) + `)`,
		// Absent cred dir keeps its literal deny (never dropped).
		`(subpath ` + sbplQuote(filepath.Join(realHome, ".aws")) + `)`,
	} {
		if !strings.Contains(profile, want) {
			t.Fatalf("profile missing %q:\n%s", want, profile)
		}
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
	sc, err := sb.Command(context.Background(), SandboxSpec{
		WorktreeDir: worktree,
		TmpDir:      os.TempDir(),
		LogDir:      logs,
	}, []string{"/usr/bin/true", "--flag"})
	if err != nil {
		t.Fatal(err)
	}
	wrapped := sc.Argv
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
	sc.Cleanup()
	if _, err := os.Stat(profilePath); !os.IsNotExist(err) {
		t.Fatalf("profile still exists after cleanup: %v", err)
	}
}
