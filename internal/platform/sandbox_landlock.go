//go:build linux

package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
// to the worktree and per-run temp dir via a hidden re-exec shim. When
// requested, credential reads are denied with leaf-hierarchy read grants:
// Landlock returns EACCES rather than masking the paths as bubblewrap does.
type LandlockSandbox struct{}

func (LandlockSandbox) Name() string { return "landlock" }

// ReadConfineEnforcement implements SandboxReadConfinement: under read
// confinement the RODirs grants are restricted to the allow-list roots (the
// strict V3 floor covers read+execute), so Landlock kernel-enforces the read
// boundary. Default policies separately carve credential paths out of their
// leaf-hierarchy grants.
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

// credentialExcludingReadRules grants every filesystem subtree except the
// credential anchors. Landlock rules are additive, so the walk descends only
// through anchor ancestors and grants their sibling leaves wholesale. If an
// ancestor cannot be enumerated, its subtree receives no grant (fail closed).
func credentialExcludingReadRules(userHome, devstrapHome string) []landlock.Rule {
	anchors := credentialAnchors(userHome, devstrapHome)
	if len(anchors) == 0 {
		return []landlock.Rule{landlock.RODirs("/")}
	}

	denied := make(map[string]struct{}, len(anchors))
	for _, anchor := range anchors {
		denied[filepath.Clean(anchor)] = struct{}{}
	}
	sep := string(os.PathSeparator)
	// grantLeaf grants read+execute on ONE carved-out sibling, choosing the
	// directory or file rule by the child's resolved type. Landlock rejects
	// directory access rights on a regular file (EINVAL — NOT the ENOENT that
	// IgnoreIfMissing suppresses), so a regular file MUST use ROFiles or the
	// whole ruleset fails. Symlinks are resolved; one whose target lands on or
	// inside a credential anchor is skipped so it cannot re-expose by an alias a
	// credential the by-path carve-out removed. Non-regular, non-dir nodes
	// (sockets/fifos/devices) are skipped — there is nothing to read-grant.
	grantLeaf := func(childPath string, entry os.DirEntry) []landlock.Rule {
		mode := entry.Type()
		if mode&os.ModeSymlink != 0 {
			real, err := filepath.EvalSymlinks(childPath)
			if err != nil {
				return nil
			}
			for anchor := range denied {
				if pathsOverlap(real, anchor) {
					return nil
				}
			}
			info, err := os.Stat(childPath)
			if err != nil {
				return nil
			}
			mode = info.Mode()
		}
		switch {
		case mode.IsDir():
			return []landlock.Rule{landlock.RODirs(childPath).IgnoreIfMissing()}
		case mode.IsRegular():
			return []landlock.Rule{landlock.ROFiles(childPath).IgnoreIfMissing()}
		default:
			return nil
		}
	}
	var walk func(string) []landlock.Rule
	walk = func(dir string) []landlock.Rule {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil
		}
		var rules []landlock.Rule
		for _, entry := range entries {
			childPath := filepath.Join(dir, entry.Name())
			if _, deny := denied[childPath]; deny {
				continue
			}
			containsDeny := false
			for denyPath := range denied {
				if strings.HasPrefix(denyPath, childPath+sep) {
					containsDeny = true
					break
				}
			}
			if containsDeny {
				rules = append(rules, walk(childPath)...)
				continue
			}
			rules = append(rules, grantLeaf(childPath, entry)...)
		}
		return rules
	}
	return walk(string(os.PathSeparator))
}

// applyLandlockPolicy maps a SandboxSpec onto stacked Landlock rulesets for
// the calling process. Default reads use leaf-hierarchy grants to omit
// credential anchors when DenySensitiveReads is set. Unlike bubblewrap masks,
// omitted Landlock paths fail with EACCES rather than appearing empty.
func applyLandlockPolicy(spec SandboxSpec) error {
	abi, err := probeLandlock()
	if err != nil {
		return err
	}
	// Read roots: allow-default unless credential anchors must be carved out,
	// or read confinement restricts reads to the allow-list. IgnoreIfMissing so
	// an absent root (e.g. /nix, /snap) does not fail the whole ruleset.
	var rules []landlock.Rule
	if spec.ReadConfine {
		for _, root := range readConfineRoots(spec) {
			rules = append(rules, landlock.RODirs(root).IgnoreIfMissing())
		}
	} else if spec.DenySensitiveReads {
		rules = credentialExcludingReadRules(spec.UserHome, spec.DevstrapHome)
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
	// The linked worktree's git storage dirs (objects/refs/logs/per-worktree
	// admin) so `git commit` succeeds — NOT the common dir's hooks/config
	// (P7-SANDBOX-01). Separate rule so a rare absent dir (no reflog yet) is
	// skipped rather than failing the whole ruleset; WithRefer for git's
	// tmp-object -> objects/xx rename promotion.
	var gitRW []string
	for _, dir := range spec.GitDirs {
		if dir != "" {
			gitRW = append(gitRW, dir)
		}
	}
	if len(gitRW) > 0 {
		rules = append(rules, landlock.RWDirs(gitRW...).WithRefer().IgnoreIfMissing())
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
	// bwrap launches the child. LookPath only needs read+execute, which the read
	// rules keep allowed post-restriction.
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("sandbox-helper: %w", err)
	}
	return syscall.Exec(path, argv, os.Environ()) //nolint:gosec // argv is the policy-checked agent command; exec-ing it is this shim's whole purpose
}
