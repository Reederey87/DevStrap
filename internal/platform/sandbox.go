package platform

import (
	"context"
	"errors"
	"fmt"
)

// SandboxSpec describes the confinement an agent child process should run
// under. Paths must be absolute; implementations resolve symlinks themselves
// (macOS Seatbelt matches on real paths, and /tmp, TMPDIR, and cloud-drive
// roots are routinely symlinks).
type SandboxSpec struct {
	// WorktreeDir is the agent worktree — the only project location the
	// child may write.
	WorktreeDir string
	// TmpDir is the child's temp directory (writable).
	TmpDir string
	// LogDir is where the generated profile file lives. It is NOT granted to
	// the child: the agent log is written by the parent process, so the child
	// only ever sees inherited pipes — keeping LogDir read-only for the child
	// prevents it from tampering with its own 0600 log or the profile.
	LogDir string
	// UserHome is the REAL user home. The agent child env repoints HOME to
	// the worktree (SECU-02), but the filesystem still contains the user's
	// dotfiles — sensitive-read denies are anchored here.
	UserHome string
	// DevstrapHome is the DevStrap home (~/.devstrap); its keys directory is
	// read-denied.
	DevstrapHome string
	// DenyNetwork blocks all network access for the child.
	DenyNetwork bool
	// DenySensitiveReads blocks reads of credential-bearing paths under
	// UserHome (.ssh, .aws, .gnupg, .config/gh, .kube, .docker) and
	// DevstrapHome/keys.
	DenySensitiveReads bool
}

// Sandbox wraps an agent argv in an OS-enforced confinement (AGEN-03 /
// P4-GIT-03). Unlike the wrapper-level argv/file policies (which are advisory
// and interpreter-bypassable), a Sandbox is enforced by the kernel for the
// child and everything it spawns.
type Sandbox interface {
	Name() string
	// Available reports whether the sandbox can be used on this host; the
	// returned error explains why not (wrapped in ErrUnsupported).
	Available() error
	// Command returns the argv wrapped in the sandbox launcher plus a cleanup
	// function (always safe to call) that removes any generated profile file.
	Command(ctx context.Context, spec SandboxSpec, argv []string) ([]string, func(), error)
}

// ErrInvalidSandboxBackend marks a sandbox backend override whose VALUE is
// invalid (e.g. a typo in DEVSTRAP_SANDBOX_BACKEND). It is deliberately
// distinct from ErrUnsupported: an unsupported host may degrade to advisory
// mode, but a mistyped explicit configuration knob must fail closed — treating
// it as a capability gap would let a typo silently disable the OS sandbox.
var ErrInvalidSandboxBackend = errors.New("invalid sandbox backend")

// NetworkEnforcement grades how completely a Sandbox can enforce
// SandboxSpec.DenyNetwork. A plain boolean overclaims: Landlock's "deny"
// covers only TCP bind/connect, which must not read as netns-grade isolation
// (adversarial review P2 — DNS/QUIC/unix-socket exfiltration stays possible).
type NetworkEnforcement int

const (
	// NetworkDenyNone means the deny cannot be kernel-enforced at all; a
	// `require`-mode run whose policy demands a network deny refuses to
	// launch.
	NetworkDenyNone NetworkEnforcement = iota
	// NetworkDenyPartialTCP denies only TCP bind/connect — UDP, QUIC, and
	// unix-domain sockets stay open (Landlock ABI >= 4). Satisfies `require`
	// but is surfaced as a warning, never as a full deny.
	NetworkDenyPartialTCP
	// NetworkDenyTotal removes the child's network entirely (bubblewrap's
	// network namespace).
	NetworkDenyTotal
)

// SandboxCapabilities is an optional interface a Sandbox implements when its
// confinement can be weaker than the platform's full-fidelity backend.
// Absence of the interface (or an empty Limitations) means full fidelity.
// Encoding this in Available()'s error would be wrong — available-but-degraded
// is not an error — and Name() is not structured.
type SandboxCapabilities interface {
	// Limitations returns human-readable degrade notes for the selected
	// backend; callers print them as one notice line.
	Limitations() []string
	// NetworkDenyEnforcement reports how completely SandboxSpec.DenyNetwork
	// will be kernel-enforced for the selected backend.
	NetworkDenyEnforcement() NetworkEnforcement
}

// UnsupportedSandbox is the explicit no-sandbox placeholder for platforms
// without an implemented adapter (macOS Seatbelt and Linux
// bubblewrap-then-landlock are implemented; see spec/14 for remaining
// slices). Callers treat Available() != nil as "run unconfined and say so".
type UnsupportedSandbox struct {
	Platform string
}

func (s UnsupportedSandbox) Name() string { return "unsupported-sandbox" }

func (s UnsupportedSandbox) Available() error {
	return fmt.Errorf("%w: no OS sandbox adapter on %s", ErrUnsupported, s.Platform)
}

func (s UnsupportedSandbox) Command(context.Context, SandboxSpec, []string) ([]string, func(), error) {
	return nil, func() {}, fmt.Errorf("%w: no OS sandbox adapter on %s", ErrUnsupported, s.Platform)
}
