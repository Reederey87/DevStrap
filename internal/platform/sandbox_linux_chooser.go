//go:build linux

package platform

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
)

// SandboxBackendEnv forces the Linux sandbox backend instead of the
// bwrap-then-landlock probe order: "bwrap" pins the full-fidelity mount
// sandbox (credential masks, total netns deny), "landlock" pins the fallback
// (useful when bwrap's user namespaces break nested-sandbox tools, and for CI
// to exercise the landlock path on hosts where bwrap works). A forced backend
// never silently falls back: its own availability error surfaces so
// `--sandbox require` sees the real reason.
const SandboxBackendEnv = "DEVSTRAP_SANDBOX_BACKEND"

// LinuxSandbox lazily selects bubblewrap, then landlock, then unsupported
// (P4-GIT-03 slice 3). Both probes are cached OnceValues, so
// Name/Available/Command converge on one decision per process and the
// expensive bwrap namespace launch still happens at most once.
type LinuxSandbox struct{}

// linuxSelection freezes both the chosen backend and whether it was forced,
// so Limitations() cannot drift from the cached decision if the env var
// changes later in the process (adversarial review P3).
type linuxSelection struct {
	sb     Sandbox
	forced bool
}

var selectLinuxSandbox = sync.OnceValues(func() (linuxSelection, error) {
	backend := os.Getenv(SandboxBackendEnv)
	sb, err := chooseLinuxSandbox(backend, BubblewrapSandbox{}, LandlockSandbox{})
	return linuxSelection{sb: sb, forced: strings.TrimSpace(backend) != ""}, err
})

// chooseLinuxSandbox is the selection core, kept free of process state so the
// matrix is unit-testable with stub sandboxes.
func chooseLinuxSandbox(backend string, bwrap, ll Sandbox) (Sandbox, error) {
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case "bwrap", "bubblewrap":
		return bwrap, bwrap.Available()
	case "landlock":
		return ll, ll.Available()
	case "":
		bwrapErr := bwrap.Available()
		if bwrapErr == nil {
			return bwrap, nil
		}
		if llErr := ll.Available(); llErr != nil {
			return nil, fmt.Errorf("no usable Linux sandbox: bwrap: %w; landlock: %w", bwrapErr, llErr)
		}
		return ll, nil
	default:
		return nil, fmt.Errorf("%w: %s=%q (want bwrap or landlock)", ErrInvalidSandboxBackend, SandboxBackendEnv, backend)
	}
}

func (LinuxSandbox) Name() string {
	if sel, err := selectLinuxSandbox(); err == nil {
		return sel.sb.Name()
	}
	return "linux-sandbox"
}

func (LinuxSandbox) Available() error {
	_, err := selectLinuxSandbox()
	return err
}

func (LinuxSandbox) Command(ctx context.Context, spec SandboxSpec, argv []string) ([]string, func(), error) {
	sel, err := selectLinuxSandbox()
	if err != nil {
		return nil, func() {}, err
	}
	return sel.sb.Command(ctx, spec, argv)
}

// Limitations implements SandboxCapabilities: empty for bubblewrap (full
// fidelity), populated with the degrade contract when the landlock fallback
// was selected — prefixed with why bwrap was passed over so the one notice
// line tells the whole story.
func (LinuxSandbox) Limitations() []string {
	sel, err := selectLinuxSandbox()
	if err != nil || sel.sb.Name() != (LandlockSandbox{}).Name() {
		return nil
	}
	reason := "unavailable"
	if sel.forced {
		reason = "backend forced via " + SandboxBackendEnv
	} else if _, bwrapErr := probeBwrap(); bwrapErr != nil {
		reason = bwrapErr.Error()
	}
	abi, _ := probeLandlock()
	return append([]string{"landlock fallback selected (bubblewrap: " + reason + ")"}, landlockLimitations(abi)...)
}

// NetworkDenyEnforcement implements SandboxCapabilities: bubblewrap's network
// namespace removes the network entirely; the landlock fallback denies only
// TCP bind/connect, and only from kernel ABI v4 on — below that a DenyNetwork
// policy would run with the network fully open, which resolveAgentSandbox
// refuses under `require`. The TCP-only grade is deliberately not reported as
// total: UDP, QUIC, and unix-domain sockets stay open under landlock.
func (LinuxSandbox) NetworkDenyEnforcement() NetworkEnforcement {
	sel, err := selectLinuxSandbox()
	if err != nil {
		return NetworkDenyNone
	}
	if sel.sb.Name() != (LandlockSandbox{}).Name() {
		return NetworkDenyTotal
	}
	if abi, _ := probeLandlock(); abi >= 4 {
		return NetworkDenyPartialTCP
	}
	return NetworkDenyNone
}
