//go:build linux

package platform

import (
	"testing"

	"github.com/elastic/go-seccomp-bpf/arch"
)

// TestSeccompFilterProgramAssembles proves the denylist compiles cleanly for
// the running arch (the arch filter drops x86-only names on arm64 so Assemble
// never fails) and that the encoded blob is a well-formed sock_filter array —
// non-empty and a whole number of 8-byte instructions.
func TestSeccompFilterProgramAssembles(t *testing.T) {
	prog, err := seccompFilterProgram()
	if err != nil {
		t.Fatalf("seccompFilterProgram: %v", err)
	}
	if len(prog) == 0 {
		t.Fatal("empty seccomp program")
	}
	if len(prog)%8 != 0 {
		t.Fatalf("program length %d is not a multiple of the 8-byte sock_filter size", len(prog))
	}
	// Round-trip: the raw instruction count derived from the byte length must
	// match the assembled program's instruction count.
	policy, err := seccompPolicy()
	if err != nil {
		t.Fatalf("seccompPolicy: %v", err)
	}
	insts, err := policy.Assemble()
	if err != nil {
		t.Fatalf("assemble policy: %v", err)
	}
	if got := len(prog) / 8; got != len(insts) {
		t.Fatalf("encoded instruction count = %d, want %d", got, len(insts))
	}
}

// TestSeccompPolicyFiltersUnknownArchNames proves the per-arch filter keeps
// only names the running arch defines — the guard that stops Assemble from
// failing on arm64, where vm86/modify_ldt/etc. do not exist.
func TestSeccompPolicyFiltersUnknownArchNames(t *testing.T) {
	info, err := arch.GetInfo("")
	if err != nil {
		t.Fatalf("arch.GetInfo: %v", err)
	}
	policy, err := seccompPolicy()
	if err != nil {
		t.Fatalf("seccompPolicy: %v", err)
	}
	if len(policy.Syscalls) != 1 {
		t.Fatalf("policy groups = %d, want 1", len(policy.Syscalls))
	}
	for _, name := range policy.Syscalls[0].Names {
		if _, ok := info.SyscallNames[name]; !ok {
			t.Errorf("policy retained %q, absent from arch %s syscall table", name, info.Name)
		}
	}
	// At least the arch-independent core (mount, ptrace, keyctl) survives.
	for _, want := range []string{"mount", "ptrace", "keyctl"} {
		if _, ok := info.SyscallNames[want]; ok && !containsName(policy.Syscalls[0].Names, want) {
			t.Errorf("arch %s defines %q but the policy dropped it", info.Name, want)
		}
	}
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}
