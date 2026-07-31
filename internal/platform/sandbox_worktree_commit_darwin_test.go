package platform

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSeatbeltAllowsLinkedWorktreeCommit is the load-bearing proof for
// P7-SANDBOX-01: a DevStrap agent worktree is a git *linked* worktree whose
// index/objects/refs live in the parent clone's .git, so the default write
// confinement (worktree + tmp only) EPERMs `git commit`. This test commits in a
// real linked worktree under the live Seatbelt sandbox and asserts it FAILS
// without the git-dir grant and SUCCEEDS with it. Env-gated like the other
// Seatbelt enforcement e2e.
func TestSeatbeltAllowsLinkedWorktreeCommit(t *testing.T) {
	if os.Getenv("DEVSTRAP_SANDBOX_E2E") != "1" {
		t.Skip("set DEVSTRAP_SANDBOX_E2E=1 to run the Seatbelt worktree-commit test")
	}
	sb := SeatbeltSandbox{}
	if err := sb.Available(); err != nil {
		t.Skipf("sandbox-exec unavailable: %v", err)
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}

	root := t.TempDir()
	clone := filepath.Join(root, "clone")
	tmpAllow := filepath.Join(root, "tmp")
	logs := filepath.Join(root, "logs")
	for _, d := range []string{clone, tmpAllow, logs} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	realGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=true", "GIT_TERMINAL_PROMPT=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	realGit(clone, "init", "-b", "main")
	realGit(clone, "config", "user.name", "t")
	realGit(clone, "config", "user.email", "t@example.com")
	if err := os.WriteFile(filepath.Join(clone, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	realGit(clone, "add", "README.md")
	realGit(clone, "commit", "-m", "init")

	worktree := filepath.Join(root, "wt")
	realGit(clone, "worktree", "add", "-b", "agent/x", "--", worktree, "main")

	// Resolve the git storage dirs the same way WorktreeSandboxWriteDirs does.
	revParse := func(flag string) string {
		cmd := exec.Command(gitBin, "rev-parse", flag)
		cmd.Dir = worktree
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("rev-parse %s: %v", flag, err)
		}
		p := strings.TrimSpace(string(out))
		if !filepath.IsAbs(p) {
			p = filepath.Join(worktree, p)
		}
		if resolved, rerr := filepath.EvalSymlinks(p); rerr == nil {
			return resolved
		}
		return filepath.Clean(p)
	}
	common := revParse("--git-common-dir")
	gitDir := revParse("--git-dir")
	gitDirs := []string{
		filepath.Join(common, "objects"),
		filepath.Join(common, "refs"),
		filepath.Join(common, "logs"),
		gitDir,
	}

	commitUnder := func(gd []string) error {
		if err := os.WriteFile(filepath.Join(worktree, "change.txt"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		spec := SandboxSpec{
			WorktreeDir: worktree,
			TmpDir:      tmpAllow,
			LogDir:      logs,
			UserHome:    root, // no real dotfiles here; keep credential denies harmless
			GitDirs:     gd,
		}
		// git add + git commit as one sandboxed shell invocation so both index
		// and object/ref writes are exercised under the same confinement.
		sc, err := sb.Command(context.Background(), spec,
			[]string{"/bin/sh", "-c", "git add change.txt && git commit -m sandboxed"})
		if err != nil {
			t.Fatal(err)
		}
		defer sc.Cleanup()
		cmd := exec.Command(sc.Argv[0], sc.Argv[1:]...) //nolint:gosec // test fixture argv
		cmd.Dir = worktree
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=true", "GIT_TERMINAL_PROMPT=0")
		return cmd.Run()
	}

	// Without the git-dir grant, the commit's index/object writes to the parent
	// clone's .git are EPERM'd — this is the P7-SANDBOX-01 bug.
	if err := commitUnder(nil); err == nil {
		t.Fatal("commit in a linked worktree SUCCEEDED without the git-dir grant; the fix is not load-bearing (or the sandbox is not enforcing)")
	}
	// git leaves a stale index.lock behind after the denied write; clear it so
	// the second attempt starts clean.
	_ = os.Remove(filepath.Join(gitDir, "index.lock"))

	// With the grant, the same commit succeeds.
	if err := commitUnder(gitDirs); err != nil {
		t.Fatalf("commit in a linked worktree FAILED with the git-dir grant: %v", err)
	}
	// Verify the commit really landed on the branch.
	out, err := exec.Command(gitBin, "-C", worktree, "log", "-1", "--pretty=%s").Output()
	if err != nil || strings.TrimSpace(string(out)) != "sandboxed" {
		t.Fatalf("expected the sandboxed commit on HEAD, got %q (err %v)", strings.TrimSpace(string(out)), err)
	}
}

// TestSeatbeltDeniesCommondirRewriteInLinkedWorktree is the load-bearing proof
// for P8-SEC-02: WorktreeSandboxWriteDirs grants a linked worktree's whole
// per-worktree admin dir writable so `git commit` works (P7-SANDBOX-01), but
// that dir contains `commondir` — the pointer to the shared .git. Without the
// GitDenyFiles fix, a sandboxed agent could rewrite commondir to point at its
// own writable directory, relocating git's entire configuration into
// attacker-controlled space so the next UNSANDBOXED git command executes a
// planted hook/fsmonitor. This asserts BOTH halves under a real
// sandbox-exec-enforced Seatbelt profile, because either alone is worthless:
// the P7-SANDBOX-01 grant must still let `git commit` succeed (the fix must
// not break the thing it is layered on top of), AND writing to commondir must
// now fail (the escape is actually closed). Env-gated like the sibling
// worktree-commit e2e.
func TestSeatbeltDeniesCommondirRewriteInLinkedWorktree(t *testing.T) {
	if os.Getenv("DEVSTRAP_SANDBOX_E2E") != "1" {
		t.Skip("set DEVSTRAP_SANDBOX_E2E=1 to run the Seatbelt commondir-deny test")
	}
	sb := SeatbeltSandbox{}
	if err := sb.Available(); err != nil {
		t.Skipf("sandbox-exec unavailable: %v", err)
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}

	root := t.TempDir()
	clone := filepath.Join(root, "clone")
	tmpAllow := filepath.Join(root, "tmp")
	logs := filepath.Join(root, "logs")
	for _, d := range []string{clone, tmpAllow, logs} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	realGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=true", "GIT_TERMINAL_PROMPT=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	realGit(clone, "init", "-b", "main")
	realGit(clone, "config", "user.name", "t")
	realGit(clone, "config", "user.email", "t@example.com")
	if err := os.WriteFile(filepath.Join(clone, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	realGit(clone, "add", "README.md")
	realGit(clone, "commit", "-m", "init")

	worktree := filepath.Join(root, "wt")
	realGit(clone, "worktree", "add", "-b", "agent/deny", "--", worktree, "main")

	revParse := func(flag string) string {
		cmd := exec.Command(gitBin, "rev-parse", flag)
		cmd.Dir = worktree
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("rev-parse %s: %v", flag, err)
		}
		p := strings.TrimSpace(string(out))
		if !filepath.IsAbs(p) {
			p = filepath.Join(worktree, p)
		}
		if resolved, rerr := filepath.EvalSymlinks(p); rerr == nil {
			return resolved
		}
		return filepath.Clean(p)
	}
	common := revParse("--git-common-dir")
	gitDir := revParse("--git-dir")
	gitDirs := []string{
		filepath.Join(common, "objects"),
		filepath.Join(common, "refs"),
		filepath.Join(common, "logs"),
		gitDir,
	}
	gitDenyFiles := []string{
		filepath.Join(gitDir, "commondir"),
		filepath.Join(gitDir, "gitdir"),
		filepath.Join(gitDir, "config.worktree"),
	}

	spec := SandboxSpec{
		WorktreeDir:  worktree,
		TmpDir:       tmpAllow,
		LogDir:       logs,
		UserHome:     root, // no real dotfiles here; keep credential denies harmless
		GitDirs:      gitDirs,
		GitDenyFiles: gitDenyFiles,
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
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=true", "GIT_TERMINAL_PROMPT=0")
		return cmd.Run()
	}

	// Half 1: the P7-SANDBOX-01 grant must still work — an ordinary commit in
	// the sandboxed linked worktree succeeds.
	if err := os.WriteFile(filepath.Join(worktree, "change.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run("/bin/sh", "-c", "git add change.txt && git commit -m sandboxed"); err != nil {
		t.Fatalf("commit in a linked worktree FAILED with the git-dir grant + deny files present: %v", err)
	}
	out, err := exec.Command(gitBin, "-C", worktree, "log", "-1", "--pretty=%s").Output()
	if err != nil || strings.TrimSpace(string(out)) != "sandboxed" {
		t.Fatalf("expected the sandboxed commit on HEAD, got %q (err %v)", strings.TrimSpace(string(out)), err)
	}

	// Half 2: writing to commondir — the actual escape primitive — must fail.
	if err := run("/bin/sh", "-c", "echo evil > "+shellQuote(filepath.Join(gitDir, "commondir"))); err == nil {
		t.Fatal("write to commondir SUCCEEDED under the sandbox; P8-SEC-02 escape is still open")
	}
	// The file's content must be unchanged from what `git worktree add` wrote —
	// a denied write must not have partially landed.
	content, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		t.Fatalf("read commondir after denied write attempt: %v", err)
	}
	if strings.Contains(string(content), "evil") {
		t.Fatalf("commondir was rewritten despite the sandbox denial: %q", content)
	}
}

// shellQuote wraps a path in single quotes for embedding in a `/bin/sh -c`
// script, escaping any literal single quote the path might contain.
func shellQuote(path string) string {
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}
