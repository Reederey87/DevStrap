package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/state"
)

type fakeSandbox struct {
	availableErr error
}

func (f fakeSandbox) Name() string     { return "fake-sandbox" }
func (f fakeSandbox) Available() error { return f.availableErr }
func (f fakeSandbox) Command(_ context.Context, _ platform.SandboxSpec, argv []string) (platform.SandboxCommand, error) {
	return platform.SandboxCommand{Argv: append([]string{"fake-sandbox-exec"}, argv...), Cleanup: func() {}}, nil
}

// The fake enforces read confinement like every real backend, so the readonly
// policy's auto read-confine resolves cleanly in the matrix tests.
func (f fakeSandbox) ReadConfineEnforcement() platform.ReadConfineEnforcement {
	return platform.ReadConfineEnforced
}

// fakeNoReadConfineSandbox is a backend that CANNOT read-confine, for the
// refuse/warn path (a hypothetical future degraded adapter).
type fakeNoReadConfineSandbox struct{ availableErr error }

func (f fakeNoReadConfineSandbox) Name() string     { return "fake-no-readconfine" }
func (f fakeNoReadConfineSandbox) Available() error { return f.availableErr }
func (f fakeNoReadConfineSandbox) Command(_ context.Context, _ platform.SandboxSpec, argv []string) (platform.SandboxCommand, error) {
	return platform.SandboxCommand{Argv: append([]string{"fake-exec"}, argv...), Cleanup: func() {}}, nil
}

type fakeCapSandbox struct {
	fakeSandbox
	limitations []string
	netEnforce  platform.NetworkEnforcement
}

func (f fakeCapSandbox) Limitations() []string { return f.limitations }
func (f fakeCapSandbox) NetworkDenyEnforcement() platform.NetworkEnforcement {
	return f.netEnforce
}

type fakeViolationSandbox struct {
	fakeSandbox
	violations []platform.SandboxViolation
}

func (f fakeViolationSandbox) CollectViolations(_ context.Context, _ string, _ time.Time) ([]platform.SandboxViolation, error) {
	return f.violations, nil
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
func (p passthroughSandbox) Command(_ context.Context, spec platform.SandboxSpec, argv []string) (platform.SandboxCommand, error) {
	*p.spec = spec
	// sh -c positional semantics: the arg after the script is $0 (the marker
	// path), the rest become "$@" (the original child argv).
	wrapped := append([]string{"sh", "-c", `touch "$0" && exec "$@"`, p.marker}, argv...)
	return platform.SandboxCommand{Argv: wrapped, Cleanup: func() { *p.cleanupRuns++ }}, nil
}

// TestAgentSandboxSpecFailsClosedWithoutUserHome pins the post-merge review
// fix on PR #107: when the real user home cannot be resolved, the run must
// fail rather than render a profile whose home-anchored credential denies
// silently vanished.
func TestAgentSandboxSpecFailsClosedWithoutUserHome(t *testing.T) {
	t.Setenv("HOME", "")
	if _, err := agentSandboxSpec("/wt", "/tmp/run", "/log", agentSandboxLaunch{devstrapHome: "/dsh"}, "arun_test"); err == nil {
		t.Fatal("agentSandboxSpec succeeded without a resolvable user home; want fail-closed error")
	}
}

func TestAgentSandboxSpecAnchorsRealUserHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	spec, err := agentSandboxSpec("/wt", "/tmp/run", "/log", agentSandboxLaunch{devstrapHome: "/dsh", denyNetwork: true}, "arun_test")
	if err != nil {
		t.Fatalf("agentSandboxSpec: %v", err)
	}
	if spec.UserHome != home {
		t.Fatalf("UserHome = %q, want the real home %q (not the worktree-repointed child HOME)", spec.UserHome, home)
	}
	if !spec.DenySensitiveReads || !spec.DenyNetwork || spec.WorktreeDir != "/wt" || spec.TmpDir != "/tmp/run" || spec.LogDir != "/log" || spec.DevstrapHome != "/dsh" {
		t.Fatalf("spec fields not threaded through: %+v", spec)
	}
	if spec.ViolationTag != "devstrap-sb-arun_test" {
		t.Fatalf("ViolationTag = %q, want devstrap-sb-arun_test", spec.ViolationTag)
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
			launch, err := resolveAgentSandbox(tc.mode, tc.policy, "auto", nil, &stderr, "/tmp/devstrap-home")
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
			if launch.mode != tc.mode {
				t.Fatalf("mode = %q, want %q", launch.mode, tc.mode)
			}
			if tc.wantEnabled && launch.backendName != tc.sandbox.Name() {
				t.Fatalf("backendName = %q, want %q", launch.backendName, tc.sandbox.Name())
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

func TestResolveAgentSandboxCapabilities(t *testing.T) {
	cases := []struct {
		name          string
		mode          string
		policy        string
		sandbox       platform.Sandbox
		wantErrCode   int
		wantErrSub    string
		wantWarnSub   string
		wantNoticeSub []string
		wantNoWarnSub string
		wantNoStderr  bool
	}{
		{
			name:        "require refuses missing network deny",
			mode:        "require",
			policy:      "readonly",
			sandbox:     fakeCapSandbox{netEnforce: platform.NetworkDenyNone},
			wantErrCode: exitPolicy,
			wantErrSub:  "requires a network deny",
		},
		{
			name:        "auto warns missing network deny",
			mode:        "auto",
			policy:      "readonly",
			sandbox:     fakeCapSandbox{netEnforce: platform.NetworkDenyNone},
			wantWarnSub: "cannot enforce the readonly network deny",
		},
		{
			name:          "guarded limitations without network warning",
			mode:          "auto",
			policy:        "guarded",
			sandbox:       fakeCapSandbox{netEnforce: platform.NetworkDenyNone, limitations: []string{"lim-a", "lim-b"}},
			wantNoWarnSub: "cannot enforce",
			wantNoticeSub: []string{"reduced guarantees", "lim-a", "lim-b"},
		},
		{
			name:          "require accepts total-deny degraded backend",
			mode:          "require",
			policy:        "readonly",
			sandbox:       fakeCapSandbox{netEnforce: platform.NetworkDenyTotal, limitations: []string{"lim-a"}},
			wantNoWarnSub: "network deny",
			wantNoticeSub: []string{"reduced guarantees"},
		},
		{
			name:          "require accepts TCP-only deny with a warning",
			mode:          "require",
			policy:        "readonly",
			sandbox:       fakeCapSandbox{netEnforce: platform.NetworkDenyPartialTCP, limitations: []string{"lim-a"}},
			wantWarnSub:   "TCP bind/connect only",
			wantNoticeSub: []string{"reduced guarantees"},
		},
		{
			name:         "plain sandbox has no capability notices",
			mode:         "auto",
			policy:       "readonly",
			sandbox:      fakeSandbox{},
			wantNoStderr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withFakeSandbox(t, tc.sandbox)
			var stderr bytes.Buffer
			launch, err := resolveAgentSandbox(tc.mode, tc.policy, "auto", nil, &stderr, "/tmp/devstrap-home")
			if tc.wantErrCode != 0 {
				var app appError
				if !errors.As(err, &app) || app.code != tc.wantErrCode {
					t.Fatalf("err = %v, want appError code %d", err, tc.wantErrCode)
				}
				if !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("err = %v, want substring %q", err, tc.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveAgentSandbox: %v", err)
			}
			if !launch.enabled {
				t.Fatal("enabled = false, want true")
			}
			if len(tc.wantNoticeSub) > 0 && len(launch.limitations) == 0 {
				t.Fatal("limitations not captured on launch")
			}
			if tc.wantWarnSub != "" && !strings.Contains(stderr.String(), tc.wantWarnSub) {
				t.Fatalf("stderr = %q, want warning substring %q", stderr.String(), tc.wantWarnSub)
			}
			if tc.wantNoWarnSub != "" && strings.Contains(stderr.String(), tc.wantNoWarnSub) {
				t.Fatalf("stderr = %q, want no substring %q", stderr.String(), tc.wantNoWarnSub)
			}
			for _, sub := range tc.wantNoticeSub {
				if !strings.Contains(stderr.String(), sub) {
					t.Fatalf("stderr = %q, want substring %q", stderr.String(), sub)
				}
			}
			if tc.wantNoStderr && stderr.String() != "" {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}

func TestSandboxTelemetryHelpers(t *testing.T) {
	if got := sandboxViolationTag("arun_123"); got != "devstrap-sb-arun_123" {
		t.Fatalf("sandboxViolationTag = %q", got)
	}
	if got := marshalLimitations(nil); got != "" {
		t.Fatalf("marshalLimitations(nil) = %q, want empty", got)
	}
	if got := marshalLimitations([]string{"lim-a", "lim-b"}); got != `["lim-a","lim-b"]` {
		t.Fatalf("marshalLimitations = %q", got)
	}
}

func TestCollectSandboxViolationsPersistsScrubbedRows(t *testing.T) {
	ctx := context.Background()
	st, err := state.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(ctx, "personal", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureDevice(ctx, "test-device"); err != nil {
		t.Fatal(err)
	}
	ns, err := st.UpsertProject(ctx, state.UpsertProjectParams{Path: "work/collect", Type: "plain_folder"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.InsertAgentRun(ctx, state.AgentRun{
		ID:          "arun_collect",
		NamespaceID: ns.ID,
		Engine:      "generic",
		Task:        "collect",
		Status:      "running",
	})
	if err != nil {
		t.Fatal(err)
	}
	sb := fakeViolationSandbox{violations: []platform.SandboxViolation{
		{Operation: "file-write-create", Path: "/tmp/outside", Detail: "deny(1) file-write-create /tmp/outside"},
		{Operation: "file-read-data", Path: "/tmp/token-ghp_123456789012345678901234567890123456", Detail: "deny(1) file-read-data /tmp/token-ghp_123456789012345678901234567890123456"},
	}}
	var stderr bytes.Buffer
	collectSandboxViolations(ctx, &stderr, st, run, agentSandboxLaunch{sandbox: sb, enabled: true, backendName: "seatbelt"}, time.Now())
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	got, err := st.SandboxViolationsByRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("violations = %+v, want 2", got)
	}
	if got[0].Backend != "seatbelt" || got[0].Source != "seatbelt-log" || got[0].Operation != "file-write-create" || got[0].Path != "/tmp/outside" {
		t.Fatalf("first violation = %+v", got[0])
	}
	if strings.Contains(got[1].Path, "ghp_") || strings.Contains(got[1].Detail, "ghp_") {
		t.Fatalf("secret-looking token was not scrubbed: %+v", got[1])
	}
}

func TestSandboxHelperCommandIsHiddenAndFailsClosed(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := NewRootCommand(&stdout, &stderr)
	var helperFound bool
	for _, cmd := range root.Commands() {
		if strings.HasPrefix(cmd.Use, platform.SandboxHelperCommand) {
			helperFound = true
			if !cmd.Hidden {
				t.Fatal("sandbox-helper command is not hidden")
			}
		}
	}
	if !helperFound {
		t.Fatal("sandbox-helper command not registered")
	}

	root.SetArgs([]string{platform.SandboxHelperCommand, "--spec", "{not-json", "--", "true"})
	err := root.Execute()
	var app appError
	if !errors.As(err, &app) || app.code != exitSandboxHelper {
		t.Fatalf("err = %v, want appError code %d", err, exitSandboxHelper)
	}
	if !strings.Contains(err.Error(), "sandbox-helper") {
		t.Fatalf("err = %v, want sandbox-helper prefix", err)
	}
}

// TestResolveAgentSandboxInvalidBackendFailsClosed pins the Codex-review P1:
// a mistyped DEVSTRAP_SANDBOX_BACKEND is explicit configuration, not a host
// capability gap — `auto` must NOT degrade to the advisory warning (which
// would let a typo silently disable the OS sandbox); every mode fails closed
// with the invalid-config exit class.
func TestResolveAgentSandboxInvalidBackendFailsClosed(t *testing.T) {
	invalid := fmt.Errorf("%w: DEVSTRAP_SANDBOX_BACKEND=\"landlok\" (want bwrap or landlock)", platform.ErrInvalidSandboxBackend)
	for _, mode := range []string{"auto", "require"} {
		t.Run(mode, func(t *testing.T) {
			withFakeSandbox(t, fakeSandbox{availableErr: invalid})
			var stderr bytes.Buffer
			_, err := resolveAgentSandbox(mode, "guarded", "auto", nil, &stderr, "/tmp/devstrap-home")
			var app appError
			if !errors.As(err, &app) || app.code != exitInvalidConfig {
				t.Fatalf("mode %s: err = %v, want appError code %d", mode, err, exitInvalidConfig)
			}
			if strings.Contains(stderr.String(), "advisory") {
				t.Fatalf("mode %s: degraded to advisory warning instead of failing closed: %q", mode, stderr.String())
			}
		})
	}
}

// TestResolveAgentSandboxInvalidSeccompToggleFailsClosed pins the same
// fail-closed parity for the seccomp escape hatch: a mistyped
// DEVSTRAP_SANDBOX_SECCOMP is explicit config, so it must fail closed with the
// invalid-config exit class in every sandboxing mode — including `auto` when
// the host sandbox is UNAVAILABLE (the degrade path must not swallow the typo,
// Codex review P3). `off` never reads the toggle, so it stays a clean run.
func TestResolveAgentSandboxInvalidSeccompToggleFailsClosed(t *testing.T) {
	unavailable := fakeSandbox{availableErr: errors.New("no adapter on this host")}
	for _, mode := range []string{"auto", "require"} {
		t.Run(mode, func(t *testing.T) {
			withFakeSandbox(t, unavailable)
			t.Setenv("DEVSTRAP_SANDBOX_SECCOMP", "yes-please")
			var stderr bytes.Buffer
			_, err := resolveAgentSandbox(mode, "guarded", "auto", nil, &stderr, "/tmp/devstrap-home")
			var app appError
			if !errors.As(err, &app) || app.code != exitInvalidConfig {
				t.Fatalf("mode %s: err = %v, want appError code %d", mode, err, exitInvalidConfig)
			}
			if strings.Contains(stderr.String(), "advisory") {
				t.Fatalf("mode %s: degraded to advisory instead of failing closed on the typo: %q", mode, stderr.String())
			}
		})
	}
	t.Run("off ignores the toggle", func(t *testing.T) {
		withFakeSandbox(t, unavailable)
		t.Setenv("DEVSTRAP_SANDBOX_SECCOMP", "yes-please")
		if _, err := resolveAgentSandbox("off", "guarded", "auto", nil, &bytes.Buffer{}, "/tmp/devstrap-home"); err != nil {
			t.Fatalf("off mode read the seccomp toggle: %v", err)
		}
	})
}

func TestResolveAgentSandboxReadConfineMatrix(t *testing.T) {
	cases := []struct {
		name        string
		mode        string
		policy      string
		readConfine string
		sandbox     platform.Sandbox
		wantConfine bool
		wantErr     bool
		wantWarn    bool
	}{
		{name: "auto+readonly enables", mode: "auto", policy: "readonly", readConfine: "auto", sandbox: fakeSandbox{}, wantConfine: true},
		{name: "auto+guarded stays off", mode: "auto", policy: "guarded", readConfine: "auto", sandbox: fakeSandbox{}, wantConfine: false},
		{name: "explicit on with guarded", mode: "auto", policy: "guarded", readConfine: "on", sandbox: fakeSandbox{}, wantConfine: true},
		{name: "explicit off with readonly", mode: "auto", policy: "readonly", readConfine: "off", sandbox: fakeSandbox{}, wantConfine: false},
		{name: "typo fails closed", mode: "auto", policy: "guarded", readConfine: "sometimes", sandbox: fakeSandbox{}, wantErr: true},
		{name: "explicit on but backend cannot enforce refuses", mode: "auto", policy: "guarded", readConfine: "on", sandbox: fakeNoReadConfineSandbox{}, wantErr: true},
		{name: "require+readonly but backend cannot enforce refuses", mode: "require", policy: "readonly", readConfine: "auto", sandbox: fakeNoReadConfineSandbox{}, wantErr: true},
		{name: "auto+readonly backend cannot enforce warns", mode: "auto", policy: "readonly", readConfine: "auto", sandbox: fakeNoReadConfineSandbox{}, wantConfine: false, wantWarn: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withFakeSandbox(t, tc.sandbox)
			var stderr bytes.Buffer
			launch, err := resolveAgentSandbox(tc.mode, tc.policy, tc.readConfine, nil, &stderr, "/tmp/devstrap-home")
			if tc.wantErr {
				var app appError
				if !errors.As(err, &app) {
					t.Fatalf("err = %v, want appError", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if launch.readConfine != tc.wantConfine {
				t.Fatalf("readConfine = %v, want %v", launch.readConfine, tc.wantConfine)
			}
			if tc.wantWarn && !strings.Contains(stderr.String(), "cannot enforce read confinement") {
				t.Fatalf("expected a read-confine warning, got %q", stderr.String())
			}
		})
	}
}
