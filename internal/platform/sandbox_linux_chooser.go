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

var selectLinuxSandbox = sync.OnceValues(func() (Sandbox, error) {
	return chooseLinuxSandbox(os.Getenv(SandboxBackendEnv), BubblewrapSandbox{}, LandlockSandbox{})
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
			return nil, fmt.Errorf("%w: no usable Linux sandbox: bwrap: %v; landlock: %v", ErrUnsupported, bwrapErr, llErr)
		}
		return ll, nil
	default:
		return nil, fmt.Errorf("invalid %s=%q (want bwrap or landlock)", SandboxBackendEnv, backend)
	}
}

func (LinuxSandbox) Name() string {
	if sb, err := selectLinuxSandbox(); err == nil {
		return sb.Name()
	}
	return "linux-sandbox"
}

func (LinuxSandbox) Available() error {
	_, err := selectLinuxSandbox()
	return err
}

func (LinuxSandbox) Command(ctx context.Context, spec SandboxSpec, argv []string) ([]string, func(), error) {
	sb, err := selectLinuxSandbox()
	if err != nil {
		return nil, func() {}, err
	}
	return sb.Command(ctx, spec, argv)
}

// Limitations implements SandboxCapabilities: empty for bubblewrap (full
// fidelity), populated with the degrade contract when the landlock fallback
// was selected — prefixed with why bwrap was passed over so the one notice
// line tells the whole story.
func (LinuxSandbox) Limitations() []string {
	sb, err := selectLinuxSandbox()
	if err != nil || sb.Name() != (LandlockSandbox{}).Name() {
		return nil
	}
	reason := "unavailable"
	if os.Getenv(SandboxBackendEnv) != "" {
		reason = "backend forced via " + SandboxBackendEnv
	} else if _, bwrapErr := probeBwrap(); bwrapErr != nil {
		reason = bwrapErr.Error()
	}
	abi, _ := probeLandlock()
	return append([]string{"landlock fallback selected (bubblewrap: " + reason + ")"}, landlockLimitations(abi)...)
}

// EnforcesNetworkDeny implements SandboxCapabilities: bubblewrap's network
// namespace is total; the landlock fallback can only deny TCP, and only from
// kernel ABI v4 on — below that a DenyNetwork policy would run with the
// network fully open, which resolveAgentSandbox refuses under `require`.
func (LinuxSandbox) EnforcesNetworkDeny() bool {
	sb, err := selectLinuxSandbox()
	if err != nil {
		return false
	}
	if sb.Name() != (LandlockSandbox{}).Name() {
		return true
	}
	abi, _ := probeLandlock()
	return abi >= 4
}
