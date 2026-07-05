package platform

import (
	"path/filepath"
	"strconv"
)

// seccompChildFD is the child fd the seccomp filter memfd is inherited as: the
// first exec.Cmd.ExtraFiles slot maps to fd 3, and bubblewrap reads
// `--seccomp 3` from it. Defined with the pure builder so the fd number the
// adapter passes in ExtraFiles and the number the builder renders stay in
// lockstep (and the builder test can reference it OS-independently).
const seccompChildFD = 3

type bwrapOptions struct {
	DisableUserns bool
	// SeccompFD is the inherited fd bwrap reads the compiled syscall denylist
	// from (--seccomp <fd>). 0 means no seccomp filter — the pure builder stays
	// unit-testable on every OS without touching a real fd.
	SeccompFD int
}

// bwrapSensitivePaths derives the Linux credential masks from the same lists
// as the Seatbelt profile. Kept build-tag-free and pure so the Linux mount
// shape is unit-tested on every platform; the adapter does the host-specific
// existence filtering because bwrap fails when asked to mount over a missing
// destination under the read-only root.
func bwrapSensitivePaths(spec SandboxSpec) (dirs, files []string) {
	if !spec.DenySensitiveReads {
		return nil, nil
	}
	if spec.UserHome != "" {
		for _, rel := range sensitiveHomeDirs {
			dirs = append(dirs, filepath.Join(spec.UserHome, rel))
		}
		for _, rel := range sensitiveHomeFiles {
			files = append(files, filepath.Join(spec.UserHome, rel))
		}
	}
	if spec.DevstrapHome != "" {
		dirs = append(dirs, filepath.Join(spec.DevstrapHome, "keys"))
	}
	return dirs, files
}

// bwrapArgs renders the bubblewrap argv prefix for a SandboxSpec. Shape:
// read-only root with targeted read-write binds and targeted credential masks
// — the Linux mount-namespace equivalent of the Seatbelt allow-default,
// deny-default-writes profile. Args are discrete argv elements, so paths with
// spaces need no quoting or escaping (unlike SBPL strings).
func bwrapArgs(spec SandboxSpec, maskDirs, maskFiles []string, opts bwrapOptions) []string {
	var args []string
	if spec.ReadConfine {
		// Read confinement: expose ONLY the allow-list roots read-only instead
		// of the whole `--ro-bind / /` filesystem, so the rest of $HOME and
		// other projects are simply not in the mount namespace. --ro-bind-try
		// never fails on an absent source (a missing /nix/snap is skipped),
		// avoiding the enumerate-and-hard-fail trap. Credential masks are
		// omitted below — every credential path is outside the allow-list, so
		// read confinement already subsumes them.
		for _, root := range readConfineRoots(spec) {
			args = append(args, "--ro-bind-try", root, root)
		}
	} else {
		args = append(args, "--ro-bind", "/", "/")
	}
	args = append(args, "--proc", "/proc", "--dev", "/dev")
	if spec.WorktreeDir != "" {
		args = append(args, "--bind", spec.WorktreeDir, spec.WorktreeDir)
	}
	if spec.TmpDir != "" {
		args = append(args, "--bind", spec.TmpDir, spec.TmpDir)
	}
	// Mount ops are processed sequentially and later mounts override earlier
	// ones, so credential masks MUST come after the read-write binds. Under
	// read confinement the credential paths are already outside the exposed
	// roots, so the masks are both redundant and unmountable (their parents may
	// not exist in the namespace) — skip them.
	if !spec.ReadConfine {
		for _, dir := range maskDirs {
			args = append(args, "--tmpfs", dir)
		}
		// Read-masking diverges from Seatbelt: hidden/empty instead of EPERM.
		// Directories become empty tmpfs mounts; files become /dev/null.
		for _, file := range maskFiles {
			args = append(args, "--ro-bind", "/dev/null", file)
		}
	}
	// Seccomp filter fd (the compiled syscall denylist) — placed before the
	// namespace/terminal/chdir args so it reads clearly as part of the sandbox
	// setup; bwrap applies it regardless of position.
	if opts.SeccompFD > 0 {
		args = append(args, "--seccomp", strconv.Itoa(opts.SeccompFD))
	}
	args = append(args, "--unshare-user")
	if opts.DisableUserns {
		// Requires --unshare-user; unsupported on bwrap < 0.8 and setuid
		// bwrap, so the Linux adapter feeds this from the probe result.
		args = append(args, "--disable-userns")
	}
	// Required for --die-with-parent to kill the whole descendant tree
	// (bubblewrap issue #529).
	args = append(args, "--unshare-pid")
	if spec.DenyNetwork {
		args = append(args, "--unshare-net")
	}
	args = append(args, "--die-with-parent")
	// CVE-2017-5226 TIOCSTI terminal-injection escape. Inherited-stdin reads
	// keep working after setsid; full TUI agents may degrade and --sandbox off
	// is the escape hatch.
	args = append(args, "--new-session")
	if spec.WorktreeDir != "" {
		args = append(args, "--chdir", spec.WorktreeDir)
	}
	// Deliberately absent: --clearenv (the parent env is already the sanitized
	// childenv allowlist and must pass through), --tmpfs /tmp (stray /tmp writes
	// should fail with EROFS instead of silently succeeding-and-vanishing, which
	// mirrors Seatbelt's EPERM), any LogDir grant (the child must not touch its
	// 0600 log), and a -- argv terminator (not documented in bwrap(1); the
	// adapter guards argv[0] instead).
	return args
}
