//go:build linux

package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/landlock-lsm/go-landlock/landlock"
	llsys "github.com/landlock-lsm/go-landlock/landlock/syscall"
)

// landlockMinABI is the strict enforcement floor. ABI v3 (kernel 6.2) adds
// TRUNCATE to the handled set: a v2-only ruleset would leave truncate(2) of
// files outside the worktree permitted on every kernel — a real
// write-confinement bypass. Every host this fallback targets (Ubuntu
// 23.10+/kernel >= 6.5, where AppArmor breaks bwrap's user namespaces) has
// v3+, and v3 includes REFER (v2), which git's cross-directory object
// promotion renames need.
const landlockMinABI = 3

// LandlockSandbox is the layered Linux fallback for hosts where bubblewrap
// cannot create namespaces (AGEN-03 / P4-GIT-03 slice 3). It confines writes
// to the worktree and per-run temp dir via a hidden re-exec shim; unlike
// bubblewrap it is additive-allow, so it deliberately does NOT deny
// credential reads (spec/18 decision) — the degrade is surfaced through
// SandboxCapabilities.
type LandlockSandbox struct{}

func (LandlockSandbox) Name() string { return "landlock" }

// ReadConfineEnforcement implements SandboxReadConfinement: under read
// confinement the additive-allow RODirs grant is restricted to the roots (the
// strict V3 floor covers read+execute), so Landlock kernel-enforces read
// confinement — and, unlike its default additive-allow reads, this finally
// gives it a credential-read boundary.
func (LandlockSandbox) ReadConfineEnforcement() ReadConfineEnforcement {
	return ReadConfineEnforced
}

var probeLandlock = sync.OnceValues(func() (int, error) {
	// One landlock_create_ruleset(LANDLOCK_CREATE_RULESET_VERSION) syscall —
	// no subprocess launch needed, unlike the bwrap probe.
	abi, err := llsys.LandlockGetABIVersion()
	if err != nil {
		return 0, fmt.Errorf("%w: landlock unavailable: %w", ErrUnsupported, err)
	}
	if abi < landlockMinABI {
		return abi, fmt.Errorf("%w: kernel landlock ABI %d < required %d (truncate confinement)", ErrUnsupported, abi, landlockMinABI)
	}
	return abi, nil
})

func (s LandlockSandbox) Available() error {
	_, err := probeLandlock()
	return err
}

func (s LandlockSandbox) Command(_ context.Context, spec SandboxSpec, argv []string) (SandboxCommand, error) {
	if len(argv) == 0 {
		return SandboxCommand{Cleanup: func() {}}, fmt.Errorf("landlock: empty argv")
	}
	if _, err := probeLandlock(); err != nil {
		return SandboxCommand{Cleanup: func() {}}, err
	}
	// Fail closed: never guess argv[0] or trust os.Args for the re-exec
	// target.
	self, err := os.Executable()
	if err != nil {
		return SandboxCommand{Cleanup: func() {}}, fmt.Errorf("landlock: resolve devstrap executable for re-exec: %w", err)
	}
	resolved, err := resolveSandboxSpecPaths(spec)
	if err != nil {
		return SandboxCommand{Cleanup: func() {}}, err
	}
	specJSON, err := json.Marshal(resolved)
	if err != nil {
		return SandboxCommand{Cleanup: func() {}}, fmt.Errorf("landlock: encode sandbox spec: %w", err)
	}
	// No profile file or inherited fd exists (the seccomp filter is loaded
	// in-process by the shim, not passed as a fd), so cleanup is a no-op; the
	// Sandbox contract explicitly permits a safe no-op cleanup.
	return SandboxCommand{Argv: sandboxHelperArgs(self, string(specJSON), argv), Cleanup: func() {}}, nil
}

// applyLandlockPolicy maps a SandboxSpec onto stacked Landlock rulesets for
// the calling process. Additive-allow by design: DenySensitiveReads is
// deliberately NOT implemented here — Landlock cannot subtract credential
// paths from a broad read grant, so read-denial stays a bubblewrap-only
// guarantee (spec/18, PR #121 follow-up) surfaced via landlockLimitations.
func applyLandlockPolicy(spec SandboxSpec) error {
	abi, err := probeLandlock()
	if err != nil {
		return err
	}
	// Read roots: allow-default (read+execute everywhere, the analogue of
	// bwrap's `--ro-bind / /`) unless read confinement restricts reads to the
	// allow-list. Landlock is additive-allow, so restricting the RODirs grant
	// to the roots IS the confinement — and because the credential dirs are not
	// among the roots, read confinement finally gives the Landlock backend a
	// credential-read boundary it otherwise lacks. IgnoreIfMissing so an absent
	// root (e.g. /nix, /snap) does not fail the whole ruleset.
	var rules []landlock.Rule
	if spec.ReadConfine {
		for _, root := range readConfineRoots(spec) {
			rules = append(rules, landlock.RODirs(root).IgnoreIfMissing())
		}
	} else {
		rules = []landlock.Rule{landlock.RODirs("/")}
	}
	var rw []string
	for _, dir := range []string{spec.WorktreeDir, spec.TmpDir} {
		if dir != "" {
			rw = append(rw, dir)
		}
	}
	if len(rw) > 0 {
		// WithRefer: reparenting (rename/link across directories) is
		// implicitly denied from ABI v2 on; git promotes objects with exactly
		// such renames inside the worktree.
		rules = append(rules, landlock.RWDirs(rw...).WithRefer())
	}
	// Shell plumbing (`> /dev/null`) and pty allocation; IgnoreIfMissing
	// because minimal containers lack some nodes. Wider than bwrap's fresh
	// --dev (these nodes are machine-shared), named in landlockLimitations.
	// LogDir is deliberately absent: the child must not touch its 0600 log.
	rules = append(rules,
		landlock.RWFiles("/dev/null", "/dev/zero", "/dev/full", "/dev/tty", "/dev/ptmx").IgnoreIfMissing(),
		landlock.RWDirs("/dev/pts", "/dev/shm").IgnoreIfMissing(),
	)
	// Strict V3, not BestEffort: BestEffort would silently no-op on old
	// kernels, and the probe already guarantees ABI >= 3. The handled set is
	// exactly V3's on every kernel, so device ioctls (V5) stay unrestricted
	// and behavior is deterministic across hosts.
	if err := landlock.V3.RestrictPaths(rules...); err != nil {
		return fmt.Errorf("landlock: restrict filesystem: %w", err)
	}
	if spec.DenyNetwork && abi >= 4 {
		// A second stacked ruleset with zero rules denies all TCP bind and
		// connect. ABI < 4 is the documented degrade — resolveAgentSandbox
		// already refused (require) or warned (auto) before launch.
		if err := landlock.V4.RestrictNet(); err != nil {
			return fmt.Errorf("landlock: restrict network: %w", err)
		}
	}
	return nil
}

// ExecSandboxHelper is the sandbox-helper body: apply the Landlock ruleset to
// THIS process (go-landlock sets no_new_privs; the ruleset persists across
// execve), then replace the image with the agent argv. Same PID, so the
// parent's exec.CommandContext kill and exit-code propagation are untouched.
// Only returns on error; the CLI maps that to the wrapper-failed exit code.
func ExecSandboxHelper(spec SandboxSpec, argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("sandbox-helper: empty argv")
	}
	for name, dir := range map[string]string{"worktree": spec.WorktreeDir, "tmp": spec.TmpDir} {
		if dir != "" && !filepath.IsAbs(dir) {
			return fmt.Errorf("sandbox-helper: non-absolute sandbox %s dir %q", name, dir)
		}
	}
	if err := applyLandlockPolicy(spec); err != nil {
		return err
	}
	// Layer the syscall denylist on top of the filesystem confinement. Loaded
	// in-process (this shim has no separate launcher to hand a fd to) after
	// Landlock and before execve, so the filter persists into the agent image.
	// A probe failure is the documented degrade — skip silently, matching the
	// bubblewrap path; a load failure is fatal (the shim's error maps to the
	// exit-125 wrapper-failed path).
	if spec.DenyDangerousSyscalls && probeSeccomp() == nil {
		if err := applySeccompSelf(); err != nil {
			return err
		}
	}
	// Resolved via the sanitized child PATH the shim inherited, matching how
	// bwrap launches the child. LookPath only needs read+execute, which the
	// broad RODirs grant keeps allowed post-restriction.
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("sandbox-helper: %w", err)
	}
	return syscall.Exec(path, argv, os.Environ()) //nolint:gosec // argv is the policy-checked agent command; exec-ing it is this shim's whole purpose
}
