package platform

import (
	"context"
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

// UnsupportedSandbox is the explicit no-sandbox placeholder for platforms
// without an implemented adapter (macOS Seatbelt and Linux bubblewrap are
// implemented; see spec/14 for remaining slices). Callers treat
// Available() != nil as "run unconfined and say so".
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
