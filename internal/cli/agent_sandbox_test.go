package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/platform"
)

type fakeSandbox struct {
	availableErr error
}

func (f fakeSandbox) Name() string     { return "fake-sandbox" }
func (f fakeSandbox) Available() error { return f.availableErr }
func (f fakeSandbox) Command(_ context.Context, _ platform.SandboxSpec, argv []string) ([]string, func(), error) {
	return append([]string{"fake-sandbox-exec"}, argv...), func() {}, nil
}

func withFakeSandbox(t *testing.T, sb platform.Sandbox) {
	t.Helper()
	prev := sandboxBackend
	sandboxBackend = func() platform.Sandbox { return sb }
	t.Cleanup(func() { sandboxBackend = prev })
}

// passthroughSandbox is a runnable success-path fake: it records the spec it
// was handed, wraps the child argv in a real `sh -c` shim that drops a marker
// file before exec'ing the original command, and counts cleanup calls. It
// exists so the sandbox-ENABLED branch of runAgentProcess (per-run TMPDIR
// create/repoint/teardown, spec building, argv wrapping, cleanup) stays
// covered by a deterministic cobra-level test on every platform — the kernel
// enforcement tests exec the platform adapters directly and never cross this
// glue (dual-review P3, PR for P4-GIT-03 slice 2).
type passthroughSandbox struct {
	spec        *platform.SandboxSpec
	marker      string
	cleanupRuns *int
}

func (p passthroughSandbox) Name() string     { return "passthrough-sandbox" }
func (p passthroughSandbox) Available() error { return nil }
func (p passthroughSandbox) Command(_ context.Context, spec platform.SandboxSpec, argv []string) ([]string, func(), error) {
	*p.spec = spec
	// sh -c positional semantics: the arg after the script is $0 (the marker
	// path), the rest become "$@" (the original child argv).
	wrapped := append([]string{"sh", "-c", `touch "$0" && exec "$@"`, p.marker}, argv...)
	return wrapped, func() { *p.cleanupRuns++ }, nil
}

// TestAgentSandboxSpecFailsClosedWithoutUserHome pins the post-merge review
// fix on PR #107: when the real user home cannot be resolved, the run must
// fail rather than render a profile whose home-anchored credential denies
// silently vanished.
func TestAgentSandboxSpecFailsClosedWithoutUserHome(t *testing.T) {
	t.Setenv("HOME", "")
	if _, err := agentSandboxSpec("/wt", "/tmp/run", "/log", agentSandboxLaunch{devstrapHome: "/dsh"}); err == nil {
		t.Fatal("agentSandboxSpec succeeded without a resolvable user home; want fail-closed error")
	}
}

func TestAgentSandboxSpecAnchorsRealUserHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	spec, err := agentSandboxSpec("/wt", "/tmp/run", "/log", agentSandboxLaunch{devstrapHome: "/dsh", denyNetwork: true})
	if err != nil {
		t.Fatalf("agentSandboxSpec: %v", err)
	}
	if spec.UserHome != home {
		t.Fatalf("UserHome = %q, want the real home %q (not the worktree-repointed child HOME)", spec.UserHome, home)
	}
	if !spec.DenySensitiveReads || !spec.DenyNetwork || spec.WorktreeDir != "/wt" || spec.TmpDir != "/tmp/run" || spec.LogDir != "/log" || spec.DevstrapHome != "/dsh" {
		t.Fatalf("spec fields not threaded through: %+v", spec)
	}
}

func TestResolveAgentSandboxMatrix(t *testing.T) {
	unavailable := fakeSandbox{availableErr: errors.New("no adapter on this host")}
	available := fakeSandbox{}

	cases := []struct {
		name        string
		mode        string
		policy      string
		sandbox     platform.Sandbox
		wantEnabled bool
		wantDenyNet bool
		wantErrCode int
		wantWarn    bool
	}{
		{name: "auto available guarded", mode: "auto", policy: "guarded", sandbox: available, wantEnabled: true},
		{name: "auto available readonly denies network", mode: "auto", policy: "readonly", sandbox: available, wantEnabled: true, wantDenyNet: true},
		{name: "auto available cautious denies network", mode: "auto", policy: "cautious", sandbox: available, wantEnabled: true, wantDenyNet: true},
		{name: "auto available ephemeral-ci keeps network", mode: "auto", policy: "ephemeral-ci", sandbox: available, wantEnabled: true},
		{name: "auto unavailable warns and degrades", mode: "auto", policy: "guarded", sandbox: unavailable, wantWarn: true},
		{name: "require unavailable is a policy error", mode: "require", policy: "guarded", sandbox: unavailable, wantErrCode: exitPolicy},
		{name: "require available", mode: "require", policy: "guarded", sandbox: available, wantEnabled: true},
		{name: "off never sandboxes", mode: "off", policy: "guarded", sandbox: available},
		{name: "yolo-local always unconfined", mode: "auto", policy: "yolo-local", sandbox: available},
		{name: "yolo-local conflicts with require", mode: "require", policy: "yolo-local", sandbox: available, wantErrCode: exitInvalidConfig},
		{name: "bogus mode is a usage error", mode: "sometimes", policy: "guarded", sandbox: available, wantErrCode: exitInvalidConfig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withFakeSandbox(t, tc.sandbox)
			var stderr bytes.Buffer
			launch, err := resolveAgentSandbox(tc.mode, tc.policy, &stderr, "/tmp/devstrap-home")
			if tc.wantErrCode != 0 {
				var app appError
				if !errors.As(err, &app) || app.code != tc.wantErrCode {
					t.Fatalf("err = %v, want appError code %d", err, tc.wantErrCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveAgentSandbox: %v", err)
			}
			if launch.enabled != tc.wantEnabled {
				t.Fatalf("enabled = %v, want %v", launch.enabled, tc.wantEnabled)
			}
			if launch.denyNetwork != tc.wantDenyNet {
				t.Fatalf("denyNetwork = %v, want %v", launch.denyNetwork, tc.wantDenyNet)
			}
			warned := strings.Contains(stderr.String(), "OS sandbox unavailable")
			if warned != tc.wantWarn {
				t.Fatalf("warn printed = %v (stderr %q), want %v", warned, stderr.String(), tc.wantWarn)
			}
			if tc.wantEnabled && launch.sandbox == nil {
				t.Fatal("enabled launch has nil sandbox")
			}
		})
	}
}

// TestAgentRunSandboxEnabledExecPath drives `agent run` through cobra with a
// recording passthrough sandbox, pinning the sandbox-enabled branch of
// runAgentProcess end-to-end: the per-run TMPDIR is created and handed to the
// adapter (and torn down after the run), the WRAPPED argv is what actually
// executes, and the adapter cleanup runs. The real-kernel siblings
// (TestSeatbeltSandboxEnforcement / TestBubblewrapSandboxEnforcement) prove
// enforcement but exec the platform adapters directly, bypassing this glue.
func TestAgentRunSandboxEnabledExecPath(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "repo.git")
	seed := filepath.Join(tmp, "seed")
	runGit(t, tmp, "init", "--bare", remote)
	runGit(t, seed, "init")
	runGit(t, seed, "config", "user.email", "devstrap@example.test")
	runGit(t, seed, "config", "user.name", "DevStrap Test")
	runGit(t, seed, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "add", "README.md")
	runGit(t, seed, "commit", "-m", "initial")
	runGit(t, seed, "remote", "add", "origin", remote)
	runGit(t, seed, "push", "origin", "main")
	runGit(t, tmp, "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/main")
	if _, stderr, err := executeForTest("--home", home, "add", "file://"+remote, "--path", "work/acme/sandboxed-repo", "--default-branch", "main"); err != nil {
		t.Fatalf("add stderr = %q err = %v", stderr, err)
	}

	var spec platform.SandboxSpec
	cleanups := 0
	marker := filepath.Join(t.TempDir(), "wrapper-ran")
	withFakeSandbox(t, passthroughSandbox{spec: &spec, marker: marker, cleanupRuns: &cleanups})

	stdout, stderr, err := executeForTest("--home", home, "agent", "run", "work/acme/sandboxed-repo", "--engine", "generic", "--task", "sandboxed write", "--sandbox", "require", "--", "touch", "sandboxed.txt")
	if err != nil {
		t.Fatalf("agent run stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if !strings.Contains(stdout, "complete") {
		t.Fatalf("agent run stdout = %q, want completion", stdout)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("wrapper marker missing — the child ran UNWRAPPED argv: %v", err)
	}
	if cleanups != 1 {
		t.Fatalf("cleanup ran %d times, want exactly 1", cleanups)
	}
	if spec.WorktreeDir == "" || !spec.DenySensitiveReads {
		t.Fatalf("recorded spec missing worktree/deny-reads: %+v", spec)
	}
	if spec.DenyNetwork {
		t.Fatalf("guarded policy must not deny network: %+v", spec)
	}
	if !strings.Contains(filepath.Base(spec.TmpDir), "devstrap-agent-") {
		t.Fatalf("TmpDir = %q, want per-run devstrap-agent-<id> dir", spec.TmpDir)
	}
	if _, err := os.Stat(spec.TmpDir); !os.IsNotExist(err) {
		t.Fatalf("per-run tmp dir %q not torn down after the run: %v", spec.TmpDir, err)
	}
}
