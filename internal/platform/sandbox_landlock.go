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

var probeLandlock = sync.OnceValues(func() (int, error) {
	// One landlock_create_ruleset(LANDLOCK_CREATE_RULESET_VERSION) syscall —
	// no subprocess launch needed, unlike the bwrap probe.
	abi, err := llsys.LandlockGetABIVersion()
	if err != nil {
		return 0, fmt.Errorf("%w: landlock unavailable: %v", ErrUnsupported, err)
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

func (s LandlockSandbox) Command(_ context.Context, spec SandboxSpec, argv []string) ([]string, func(), error) {
	if len(argv) == 0 {
		return nil, func() {}, fmt.Errorf("landlock: empty argv")
	}
	if _, err := probeLandlock(); err != nil {
		return nil, func() {}, err
	}
	// Fail closed: never guess argv[0] or trust os.Args for the re-exec
	// target.
	self, err := os.Executable()
	if err != nil {
		return nil, func() {}, fmt.Errorf("landlock: resolve devstrap executable for re-exec: %w", err)
	}
	resolved, err := resolveSandboxSpecPaths(spec)
	if err != nil {
		return nil, func() {}, err
	}
	specJSON, err := json.Marshal(resolved)
	if err != nil {
		return nil, func() {}, fmt.Errorf("landlock: encode sandbox spec: %w", err)
	}
	// No profile file exists, so cleanup is a no-op; the Sandbox contract
	// explicitly permits a safe no-op cleanup.
	return sandboxHelperArgs(self, string(specJSON), argv), func() {}, nil
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
	// Allow-default reads and execute everywhere — the read-only-root
	// analogue of bwrap's `--ro-bind / /`.
	rules := []landlock.Rule{landlock.RODirs("/")}
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
	// Resolved via the sanitized child PATH the shim inherited, matching how
	// bwrap launches the child. LookPath only needs read+execute, which the
	// broad RODirs grant keeps allowed post-restriction.
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("sandbox-helper: %w", err)
	}
	return syscall.Exec(path, argv, os.Environ())
}
