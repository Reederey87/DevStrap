//go:build linux

package platform

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sync"

	seccomp "github.com/elastic/go-seccomp-bpf"
	"github.com/elastic/go-seccomp-bpf/arch"
	"golang.org/x/net/bpf"
	"golang.org/x/sys/unix"
)

// seccompPolicy builds the ActionAllow-default policy whose one ActionErrno
// (EPERM) group is the arch-filtered denylist. The filter MUST be assembled
// for the running arch: elastic/go-seccomp-bpf gates the whole program on the
// audit arch (a mismatch falls through to the default Allow, never crashes),
// and its Assemble FAILS — it does not skip — on any name absent from the
// arch's syscall table. Several denied names are x86-only (vm86, modify_ldt,
// _sysctl, uselib, ustat), so we drop names the running arch does not define
// before assembling; without this, assembly would error on arm64.
func seccompPolicy() (seccomp.Policy, error) {
	info, err := arch.GetInfo("")
	if err != nil {
		return seccomp.Policy{}, fmt.Errorf("seccomp: resolve native arch: %w", err)
	}
	names := make([]string, 0, len(seccompDeniedSyscalls))
	for _, name := range seccompDeniedSyscalls {
		if _, ok := info.SyscallNames[name]; ok {
			names = append(names, name)
		}
	}
	return seccomp.Policy{
		DefaultAction: seccomp.ActionAllow,
		Syscalls: []seccomp.SyscallGroup{{
			Names:  names,
			Action: seccomp.ActionErrno, // Ret() ORs in EPERM.
		}},
	}, nil
}

// seccompFilterProgram assembles the policy into the raw sock_filter byte blob
// bubblewrap's --seccomp fd expects: 8 bytes per cBPF instruction in host byte
// order (u16 code, u8 jt, u8 jf, u32 k).
func seccompFilterProgram() ([]byte, error) {
	policy, err := seccompPolicy()
	if err != nil {
		return nil, err
	}
	insts, err := policy.Assemble()
	if err != nil {
		return nil, fmt.Errorf("seccomp: assemble policy: %w", err)
	}
	raw, err := bpf.Assemble(insts)
	if err != nil {
		return nil, fmt.Errorf("seccomp: assemble BPF: %w", err)
	}
	// struct sock_filter is native-endian; the kernel and bwrap both read it in
	// host byte order.
	buf := make([]byte, 0, len(raw)*8)
	var word [8]byte
	for _, ins := range raw {
		binary.NativeEndian.PutUint16(word[0:2], ins.Op)
		word[2] = ins.Jt
		word[3] = ins.Jf
		binary.NativeEndian.PutUint32(word[4:8], ins.K)
		buf = append(buf, word[:]...)
	}
	return buf, nil
}

// seccompProgramFile writes the compiled filter to an anonymous memfd rewound
// to offset 0, ready to hand bubblewrap as an inherited fd. MFD_CLOEXEC is
// safe: exec.Cmd re-dups ExtraFiles into the child without close-on-exec, so
// the launcher still inherits it while unrelated parent execs do not.
func seccompProgramFile() (*os.File, error) {
	prog, err := seccompFilterProgram()
	if err != nil {
		return nil, err
	}
	fd, err := unix.MemfdCreate("devstrap-seccomp", unix.MFD_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("seccomp: memfd_create: %w", err)
	}
	f := os.NewFile(uintptr(fd), "devstrap-seccomp")
	if _, err := f.Write(prog); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("seccomp: write filter to memfd: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("seccomp: rewind memfd: %w", err)
	}
	return f, nil
}

// applySeccompSelf loads the denylist into the calling process with
// no_new_privs and TSYNC, so the filter covers every thread and persists
// across the shim's execve. Used by the Landlock re-exec shim, which cannot
// pass a fd to a separate launcher.
func applySeccompSelf() error {
	policy, err := seccompPolicy()
	if err != nil {
		return err
	}
	if err := seccomp.LoadFilter(seccomp.Filter{
		NoNewPrivs: true,
		Flag:       seccomp.FilterFlagTSync,
		Policy:     policy,
	}); err != nil {
		return fmt.Errorf("seccomp: load filter: %w", err)
	}
	return nil
}

// seccompSupported caches the one cheap kernel probe. Unsupported is a
// documented limitation, NOT unavailability: the fs/network confinement is
// intact, so a kernel without seccomp-filter support still sandboxes — it just
// skips the syscall denylist and says so via Limitations().
var seccompSupported = sync.OnceValues(func() (bool, error) {
	if !seccomp.Supported() {
		return false, fmt.Errorf("%w: kernel does not support seccomp filters", ErrUnsupported)
	}
	return true, nil
})

// probeSeccomp reports whether the kernel can install a seccomp filter.
func probeSeccomp() error {
	_, err := seccompSupported()
	return err
}
