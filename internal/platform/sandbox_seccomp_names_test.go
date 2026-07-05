package platform

import (
	"slices"
	"testing"
)

// TestSeccompDeniedSyscallNames pins the denylist: the escape-primitive groups
// must all be present, and the deliberately-allowed syscalls (nested-sandbox
// primitives, the agent's own process launches, and ioctl) must NOT be denied
// — denying any of them would break legitimate agent tooling.
func TestSeccompDeniedSyscallNames(t *testing.T) {
	denied := make(map[string]bool, len(seccompDeniedSyscalls))
	for _, name := range seccompDeniedSyscalls {
		if denied[name] {
			t.Fatalf("duplicate syscall %q in denylist", name)
		}
		denied[name] = true
	}

	// A representative from each rationale group must be denied.
	for _, want := range []string{
		"mount", "pivot_root", "chroot", // mount plane
		"init_module", "kexec_load", "reboot", // kernel/module/boot
		"ptrace", "process_vm_readv", "bpf", "perf_event_open", // tracing
		"add_key", "request_key", "keyctl", // keyring
		"open_by_handle_at", "userfaultfd", // escape primitives
		"io_uring_setup", "io_uring_enter", "io_uring_register", // io_uring
	} {
		if !denied[want] {
			t.Errorf("expected %q on the denylist", want)
		}
	}

	// Deliberately allowed: denying these would break nested sandboxes
	// (clone/unshare/setns), the agent's own execs (execve/fork), file I/O
	// (openat), or device control (ioctl, which TIOCSTI aside is arg-filtered
	// elsewhere).
	for _, forbidden := range []string{
		"clone", "clone3", "unshare", "setns",
		"execve", "execveat", "fork", "vfork",
		"openat", "openat2", "open", "ioctl",
	} {
		if denied[forbidden] {
			t.Errorf("%q must NOT be on the denylist (breaks legitimate agent tooling)", forbidden)
		}
	}

	// The x86-only names are intentionally listed (the assembler filters them
	// per-arch); guard that they stay grouped with the escape primitives.
	for _, x86 := range []string{"vm86", "vm86old", "modify_ldt", "_sysctl", "uselib", "ustat"} {
		if !slices.Contains(seccompDeniedSyscalls, x86) {
			t.Errorf("expected x86-only escape primitive %q in the denylist", x86)
		}
	}
}
