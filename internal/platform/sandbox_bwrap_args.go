package platform

import "path/filepath"

type bwrapOptions struct{ DisableUserns bool }

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
	args := []string{
		"--ro-bind", "/", "/",
		"--proc", "/proc",
		"--dev", "/dev",
	}
	if spec.WorktreeDir != "" {
		args = append(args, "--bind", spec.WorktreeDir, spec.WorktreeDir)
	}
	if spec.TmpDir != "" {
		args = append(args, "--bind", spec.TmpDir, spec.TmpDir)
	}
	// Mount ops are processed sequentially and later mounts override earlier
	// ones, so credential masks MUST come after the read-write binds.
	for _, dir := range maskDirs {
		args = append(args, "--tmpfs", dir)
	}
	// Read-masking diverges from Seatbelt: hidden/empty instead of EPERM.
	// Directories become empty tmpfs mounts; files become /dev/null.
	for _, file := range maskFiles {
		args = append(args, "--ro-bind", "/dev/null", file)
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
