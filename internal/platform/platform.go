package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/zalando/go-keyring"
)

// NoKeychainEnv, when set to "1", forces the file-backed key store instead of
// the OS keychain. This is required for headless/CI runs and hermetic
// end-to-end tests, where an OS keychain call can block on a GUI unlock prompt.
const NoKeychainEnv = "DEVSTRAP_NO_KEYCHAIN"

var (
	ErrUnsupported    = errors.New("platform feature unsupported")
	ErrEditorNotFound = errors.New("editor command not found")
	ErrSecretNotFound = errors.New("platform secret not found")
)

type FSEventKind string

const (
	FSEventUnknown FSEventKind = "unknown"
	FSEventScan    FSEventKind = "scan"
)

type FSEvent struct {
	Kind FSEventKind
	Path string
	At   time.Time
}

type Watcher interface {
	Name() string
	Watch(ctx context.Context, root string, events chan<- FSEvent) error
}

type ServiceSpec struct {
	Label       string
	Description string
	ExecPath    string
	Args        []string
	Env         map[string]string
	// WorkingDir, when set, is the service's working directory.
	WorkingDir string
	// StdoutPath/StderrPath are launchd-only log destinations; systemd routes to
	// journald and ignores them.
	StdoutPath string
	StderrPath string
	// RestartOnFailure asks the supervisor to relaunch the service when it exits
	// with a failure; RestartDelaySeconds throttles the relaunch (0 → adapter
	// default of 30s).
	RestartOnFailure    bool
	RestartDelaySeconds int
}

type ServiceStatus struct {
	Installed bool
	Running   bool
	Detail    string
	// UnitPath is the on-disk plist/unit file backing the service.
	UnitPath string
}

type ServiceManager interface {
	Name() string
	// DefaultLabel is the OS-idiomatic label used when the caller does not pass
	// one (a reverse-DNS launchd label, a bare systemd unit name).
	DefaultLabel() string
	// Install returns OS-idiomatic advisory notes (e.g. the Linux linger note)
	// alongside any error, so the CLI never has to branch on the OS.
	Install(ctx context.Context, spec ServiceSpec) (notes []string, err error)
	// Uninstall returns OS-idiomatic advisory notes (mirroring Install) so the
	// CLI never has to branch on the OS; the unit/plist file is always removed
	// even when the OS manager is unreachable (the headless/SSH case).
	Uninstall(ctx context.Context, label string) (notes []string, err error)
	Status(ctx context.Context, label string) (ServiceStatus, error)
}

type Keychain interface {
	Name() string
	Store(ctx context.Context, service, account string, secret []byte) error
	Load(ctx context.Context, service, account string) ([]byte, error)
	Delete(ctx context.Context, service, account string) error
}

type EditorAdapter interface {
	Name() string
	Open(ctx context.Context, dir, editor string) error
}

type Set struct {
	OS       string
	Watcher  Watcher
	Service  ServiceManager
	Keychain Keychain
	Editor   EditorAdapter
	Sandbox  Sandbox
}

func newSet(goos string, watcher Watcher, service ServiceManager, keychain Keychain, sandbox Sandbox) Set {
	if os.Getenv(NoKeychainEnv) == "1" {
		keychain = UnsupportedKeychain{Platform: goos, Target: "disabled"}
	}
	return Set{
		OS:       goos,
		Watcher:  watcher,
		Service:  service,
		Keychain: keychain,
		Editor:   SystemEditor{},
		Sandbox:  sandbox,
	}
}

type PollWatcher struct {
	Interval time.Duration
}

func (w PollWatcher) Name() string { return "poll" }

func (w PollWatcher) Watch(ctx context.Context, root string, events chan<- FSEvent) error {
	interval := w.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case at := <-timer.C:
			select {
			case events <- FSEvent{Kind: FSEventScan, Path: root, At: at}:
			case <-ctx.Done():
				return ctx.Err()
			}
			timer.Reset(interval)
		}
	}
}

type UnsupportedServiceManager struct {
	Platform string
	Target   string
}

func (m UnsupportedServiceManager) Name() string {
	if m.Target != "" {
		return m.Target
	}
	return "unsupported-service"
}

func (m UnsupportedServiceManager) DefaultLabel() string { return "devstrap-run-loop" }

func (m UnsupportedServiceManager) Install(context.Context, ServiceSpec) ([]string, error) {
	return nil, fmt.Errorf("%w: %s service manager is not implemented", ErrUnsupported, m.Name())
}

func (m UnsupportedServiceManager) Uninstall(context.Context, string) ([]string, error) {
	return nil, fmt.Errorf("%w: %s service manager is not implemented", ErrUnsupported, m.Name())
}

func (m UnsupportedServiceManager) Status(context.Context, string) (ServiceStatus, error) {
	return ServiceStatus{}, fmt.Errorf("%w: %s service manager is not implemented", ErrUnsupported, m.Name())
}

type UnsupportedKeychain struct {
	Platform string
	Target   string
}

func (k UnsupportedKeychain) Name() string {
	if k.Target != "" {
		return k.Target
	}
	return "unsupported-keychain"
}

func (k UnsupportedKeychain) Store(context.Context, string, string, []byte) error {
	return fmt.Errorf("%w: %s keychain is not implemented", ErrUnsupported, k.Name())
}

func (k UnsupportedKeychain) Load(context.Context, string, string) ([]byte, error) {
	return nil, fmt.Errorf("%w: %s keychain is not implemented", ErrUnsupported, k.Name())
}

func (k UnsupportedKeychain) Delete(context.Context, string, string) error {
	return fmt.Errorf("%w: %s keychain is not implemented", ErrUnsupported, k.Name())
}

type SystemKeychain struct {
	Platform string
	Target   string
}

func (k SystemKeychain) Name() string {
	if k.Target != "" {
		return k.Target
	}
	return "system-keychain"
}

func (k SystemKeychain) Store(_ context.Context, service, account string, secret []byte) error {
	if err := keyring.Set(service, account, string(secret)); err != nil {
		return mapKeyringError(err, k.Name())
	}
	return nil
}

func (k SystemKeychain) Load(_ context.Context, service, account string) ([]byte, error) {
	secret, err := keyring.Get(service, account)
	if err != nil {
		return nil, mapKeyringError(err, k.Name())
	}
	return []byte(secret), nil
}

func (k SystemKeychain) Delete(_ context.Context, service, account string) error {
	if err := keyring.Delete(service, account); err != nil {
		return mapKeyringError(err, k.Name())
	}
	return nil
}

func mapKeyringError(err error, name string) error {
	switch {
	case errors.Is(err, keyring.ErrUnsupportedPlatform):
		return fmt.Errorf("%w: %s keychain is not available: %w", ErrUnsupported, name, err)
	case errors.Is(err, keyring.ErrNotFound):
		return fmt.Errorf("%w: %s/%w", ErrSecretNotFound, name, err)
	case secretServiceUnreachable(err):
		// The Linux Secret Service is reached over the D-Bus session bus, which
		// go-keyring surfaces as an untyped godbus error (not ErrUnsupportedPlatform)
		// when the session bus is missing — the common headless/CI/systemd-unit
		// case. Classify it as backend-unavailable here, at the layer closest to
		// the library, so callers can rely on errors.Is(err, ErrUnsupported)
		// instead of re-deriving it from the error string. This is what lets
		// device-key custody fail closed (never mint a divergent key) rather than
		// silently downgrade to the file store (P6-XP-04).
		return fmt.Errorf("%w: %s Secret Service unreachable: %w", ErrUnsupported, name, err)
	default:
		return fmt.Errorf("%s keychain: %w", name, err)
	}
}

// secretServiceUnreachable reports whether a go-keyring error means the Linux
// Secret Service / D-Bus session bus could not be reached (as opposed to a
// genuine store/lookup failure against a live service). go-keyring returns
// these as untyped godbus errors, so string inspection is unavoidable — but it
// belongs here, at the platform seam, not in higher-level custody code
// (P6-XP-04).
func secretServiceUnreachable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	// Deliberately NARROW (CodeRabbit, PR #62): only the missing-session-bus /
	// missing-service signatures qualify. A bare "dbus" or the interface name
	// alone also appears in errors from a LIVE Secret Service (timeouts,
	// dismissed prompts), and classifying those as unavailable would let a
	// transient failure record file custody at first init or divert
	// custody-unset reads to the file store.
	for _, needle := range []string{
		"session bus",         // godbus: couldn't determine address of session bus
		"connection refused",  // dialing the session-bus socket
		"was not provided",    // dbus: org.freedesktop.secrets was not provided
		"not provided by any", // "...not provided by any .service files"
		"no such interface",   // service object missing the Secret Service interface
		"dial unix",           // dead session-bus socket: "dial unix <path>: connect: ..."
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}
