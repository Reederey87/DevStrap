package platform

// seccompDeniedSyscalls is the syscall denylist installed by both Linux
// sandbox backends (SandboxSpec.DenyDangerousSyscalls). Each denied syscall
// returns EPERM; the default action stays Allow, so this is a targeted
// denylist, not an allowlist. Kept build-tag-free and pure so the golden set
// is unit-tested on every platform; the Linux assembler filters this list to
// the running arch's syscall table before compiling (see seccompPolicy).
//
// Names must be spelled as elastic/go-seccomp-bpf's per-arch tables spell them.
// Several entries (vm86, vm86old, modify_ldt, _sysctl, uselib, ustat) exist
// only on x86 and are absent from arm64 — the assembler drops any name the
// running arch does not define rather than failing, so listing them here is
// safe on both.
var seccompDeniedSyscalls = []string{
	// Mount plane: remounting or pivoting the namespace is how a child would
	// escape the read-only-root / worktree-only write confinement.
	"mount", "umount", "umount2", "move_mount", "fsopen", "fsconfig",
	"fsmount", "fspick", "open_tree", "mount_setattr", "pivot_root", "chroot",

	// Kernel/module/boot: loading modules, kexec, reboot, and swap/accounting
	// changes are host-level operations an agent worktree never needs.
	"init_module", "finit_module", "delete_module", "kexec_load",
	"kexec_file_load", "reboot", "swapon", "swapoff", "acct",

	// Tracing: ptrace and cross-process memory/perf access let a child read or
	// hijack sibling processes, defeating per-process confinement.
	"ptrace", "process_vm_readv", "process_vm_writev", "perf_event_open",
	"bpf", "lookup_dcookie",

	// Keyring: the kernel keyring is a shared credential store outside the
	// filesystem masks.
	"add_key", "request_key", "keyctl",

	// Escape primitives: file-handle reopen, userfaultfd, and legacy
	// personality/ldt/sysctl surfaces are recurring sandbox-bypass vectors.
	"open_by_handle_at", "userfaultfd", "personality", "uselib", "ustat",
	"_sysctl", "vm86", "vm86old", "modify_ldt",

	// io_uring: an async-syscall submission ring bypasses seccomp on the
	// individual operations it batches, so the setup path itself is denied.
	"io_uring_setup", "io_uring_enter", "io_uring_register",

	// Deliberately NOT denied: clone/clone3/unshare/setns keep working so
	// nested sandboxes and Chrome-style helper sandboxes still launch;
	// execve/execveat/fork are the agent's own process launches; ioctl is not
	// arg-filtered here, so TIOCSTI terminal injection stays covered by
	// bubblewrap's --new-session on the bwrap path and is a documented gap on
	// the Landlock path (no --new-session equivalent).
}
