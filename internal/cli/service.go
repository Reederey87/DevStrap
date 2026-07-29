package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/cobra"
)

// serviceBackend resolves the platform service manager; a test seam like
// agent.go's sandboxBackend.
var serviceBackend = func() platform.ServiceManager { return platform.Detect().Service }

// Service modes, as recorded in the baked argv and parsed back by
// platform.ServiceManager.Status.
//
// Both modes install under the SAME label (platform.ServiceManager's
// DefaultLabel, historically named after run-loop). That is deliberate: one
// label means one convergence service, so switching modes replaces the unit
// rather than leaving two of them converging against the same state home. The
// label therefore identifies the convergence service, not the mode it runs —
// `service status` reports the mode separately.
const (
	serviceModeRunLoop = "run-loop"
	serviceModeDaemon  = "daemon"
)

// newServiceCommand implements `devstrap service install|uninstall|status`
// (P4-PROD-04): it wraps the existing `run-loop` (default) or `daemon start`
// (`--daemon`) in a per-user launchd LaunchAgent (macOS) or systemd user
// service (Linux) so the workspace converges unattended.
func newServiceCommand(stdout io.Writer, opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Install background convergence as an OS service",
	}
	cmd.AddCommand(newServiceInstallCommand(stdout, opts))
	cmd.AddCommand(newServiceUninstallCommand(stdout, opts))
	cmd.AddCommand(newServiceStatusCommand(stdout, opts))
	return cmd
}

// serviceInstallResult is the --json shape for `devstrap service install`
// (P5-CLI-01 part B). The stderr confirmation lines stay unchanged (P7-CLI-03,
// deliberately not gated by --quiet); this is purely additive for --json.
type serviceInstallResult struct {
	Manager  string   `json:"manager"`
	Label    string   `json:"label"`
	Mode     string   `json:"mode,omitempty"`
	UnitPath string   `json:"unit_path,omitempty"`
	Notes    []string `json:"notes,omitempty"`
}

func newServiceInstallCommand(stdout io.Writer, opts *options) *cobra.Command {
	var interval time.Duration
	var namespaceOnly bool
	var hubFile string
	var label string
	var execPath string
	var allowKeychainCustody bool
	var daemonMode bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install and start the background convergence service",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			stderr := cmd.ErrOrStderr()
			// A non-positive interval means OPPOSITE things to the two supervised
			// commands: runLoopForever clamps it up to 5m, while the daemon's
			// runPeriodic treats it as "on-demand only" and never converges. An
			// unattended service that silently never converges is the worse
			// failure and the harder one to notice, so refuse rather than pick a
			// mode-dependent meaning. `daemon start --interval 0` is still
			// available directly for an on-demand daemon.
			if interval <= 0 {
				return appError{code: exitUsage, err: fmt.Errorf("--interval must be positive for a background service (got %s); run `devstrap daemon start --interval 0` directly if you want an on-demand daemon with no periodic convergence", interval)}
			}
			// A service that cannot resolve a hub would relaunch and fail on
			// every tick; refuse up front with the same remedy run-loop uses.
			if err := hubConfigured(opts, hubFile); err != nil {
				return appError{code: exitInvalidConfig, err: err}
			}
			mgr := serviceBackend()
			forceFileCustody, err := checkServiceInstallCustody(cmd.Context(), stderr, opts, mgr, allowKeychainCustody)
			if err != nil {
				return err
			}
			resolvedExec, err := resolveServiceExecPath(execPath)
			if err != nil {
				return err
			}
			var bakedArgs []string
			if daemonMode {
				bakedArgs, err = serviceDaemonArgs(cmd, opts, interval, namespaceOnly, hubFile)
			} else {
				bakedArgs, err = serviceRunLoopArgs(cmd, opts, interval, namespaceOnly, hubFile)
			}
			if err != nil {
				return err
			}
			resolvedLabel := label
			if resolvedLabel == "" {
				resolvedLabel = mgr.DefaultLabel()
			}
			wantMode := serviceModeRunLoop
			if daemonMode {
				wantMode = serviceModeDaemon
			}
			// Observe the prior mode BEFORE Install overwrites the unit, but do
			// not announce the replacement until Install has actually succeeded —
			// on an unsupported/headless manager Install fails and no replacement
			// happened. A Status error is ignored on purpose: not being able to
			// read a prior unit is not a reason to refuse a fresh install.
			priorMode := ""
			if prior, perr := mgr.Status(cmd.Context(), resolvedLabel); perr == nil && prior.Installed {
				priorMode = prior.Mode
			}
			var serviceEnv map[string]string
			if forceFileCustody {
				serviceEnv = map[string]string{platform.NoKeychainEnv: "1"}
			}
			logDir := opts.paths().LogDir()
			description := "DevStrap run-loop (scan + sync + materialize)"
			stdoutPath := filepath.Join(logDir, "run-loop.out.log")
			stderrPath := filepath.Join(logDir, "run-loop.err.log")
			if daemonMode {
				description = "DevStrap daemon (socket API + watcher + periodic convergence)"
				stdoutPath = filepath.Join(logDir, "devstrapd.out.log")
				stderrPath = filepath.Join(logDir, "devstrapd.err.log")
			}
			spec := platform.ServiceSpec{
				Label:       resolvedLabel,
				Description: description,
				ExecPath:    resolvedExec,
				Args:        bakedArgs,
				StdoutPath:  stdoutPath,
				StderrPath:  stderrPath,
				// Coupled to run-loop's own consecutive-failure ceiling — see the
				// note by runLoopMaxConsecutiveFailures. Env stays nil unless the
				// explicit non-secret file-custody override must survive into the
				// service; adapters add PATH, and no secret enters a service file.
				// In --daemon mode the coupling does not apply: `daemon start`
				// never exits on convergence failure — it backs off internally
				// to a 30m cap and keeps serving reads. A restart here therefore
				// means a genuine crash, never a convergence outage; convergence
				// health lives on /v1/health, not in the supervisor's restart count.
				Env:                 serviceEnv,
				RestartOnFailure:    true,
				RestartDelaySeconds: 30,
			}
			notes, err := mgr.Install(cmd.Context(), spec)
			if err != nil {
				if errors.Is(err, platform.ErrUnsupported) {
					return appError{code: exitGeneric, err: fmt.Errorf("background service is not supported on this platform/session: %w", err)}
				}
				return err
			}
			// Terminal confirmation of a completed state change, deliberately not gated by --quiet (P7-CLI-03).
			_, _ = fmt.Fprintf(stderr, "installed %s service %q (%s mode)\n", mgr.Name(), resolvedLabel, wantMode)
			if priorMode != "" && priorMode != wantMode {
				// One label, one convergence service: the install above replaced
				// the other mode's unit rather than adding a second one, which
				// would double-converge against the same state home.
				_, _ = fmt.Fprintf(stderr, "replaced the previous %s-mode unit under the same label\n", priorMode)
			}
			unitPath := ""
			if status, serr := mgr.Status(cmd.Context(), resolvedLabel); serr == nil && status.UnitPath != "" {
				unitPath = status.UnitPath
				opts.progressf(stderr, "unit: %s\n", status.UnitPath)
			}
			opts.progressf(stderr, "logs: %s, %s\n", spec.StdoutPath, spec.StderrPath)
			// Notes are operator advisories (e.g. the Linux linger caveat), not
			// mere progress — print them verbatim even under --quiet.
			for _, note := range notes {
				_, _ = fmt.Fprintln(stderr, note)
			}
			return opts.render(stdout, func(w io.Writer) error { return nil }, serviceInstallResult{
				Manager:  mgr.Name(),
				Label:    resolvedLabel,
				Mode:     wantMode,
				UnitPath: unitPath,
				Notes:    notes,
			})
		},
	}
	cmd.Flags().DurationVar(&interval, "interval", 5*time.Minute, "convergence interval for the supervised command (run-loop, or daemon start under --daemon)")
	cmd.Flags().BoolVar(&namespaceOnly, "namespace-only", false, "sync namespace metadata only; skip materialization")
	cmd.Flags().StringVar(&hubFile, "hub-file", "", "file-backed test hub path")
	cmd.Flags().StringVar(&label, "label", "", "service label (defaults to the OS-idiomatic label)")
	cmd.Flags().StringVar(&execPath, "exec-path", "", "absolute path to the devstrap binary the service runs (defaults to this binary)")
	cmd.Flags().BoolVar(&allowKeychainCustody, "allow-keychain-custody", false, "allow a systemd user service to use recorded keychain custody")
	cmd.Flags().BoolVar(&daemonMode, "daemon", false, "supervise `devstrap daemon start` instead of `run-loop`: adds a local socket API and a filesystem watcher for sub-interval convergence; the daemon never exits on convergence failure, so a restart means a crash, not a failed sync")
	return cmd
}

func checkServiceInstallCustody(ctx context.Context, stderr io.Writer, opts *options, mgr platform.ServiceManager, allowKeychainCustody bool) (bool, error) {
	// The explicit session override must survive into the unit REGARDLESS of
	// store state — a pre-init install with the override set would otherwise
	// bake a unit whose runtime custody differs from the installing session
	// once init later records keychain custody (Codex review).
	overrideSet := os.Getenv(platform.NoKeychainEnv) == "1"
	paths := opts.paths()
	if _, err := os.Stat(paths.StateDB()); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Preserve the pre-init install behavior; hub/exec validation remains
			// authoritative when no state store exists yet.
			return overrideSet, nil
		}
		return false, err
	}
	store, err := opts.openState(ctx)
	if err != nil {
		return false, err
	}
	defer closeStore(store)
	if _, err := store.WorkspaceID(ctx); err != nil {
		if errors.Is(err, state.ErrNotInitialized) {
			return overrideSet, nil
		}
		return false, err
	}
	recorded, err := store.KeyCustody(ctx)
	if err != nil {
		return false, err
	}
	// An unknown recorded value is corrupt state, not a shrug: HybridStore
	// would treat it as keychain-preferred-but-not-required, silently
	// re-enabling the file fallback the custody model forbids (Codex review).
	switch recorded {
	case devicekeys.CustodyUnset, devicekeys.CustodyFile, devicekeys.CustodyKeychain:
	default:
		return false, appError{code: exitInvalidConfig, err: fmt.Errorf(
			"recorded key custody %q is not a known value (file/keychain); the store looks corrupt — re-run `devstrap init` to re-record custody before installing the service", recorded)}
	}
	// Warn on an unrecorded store BEFORE the effective-file early return, so
	// the pre-P6-XP-04 remedy is not silenced by DEVSTRAP_NO_KEYCHAIN=1.
	if recorded == devicekeys.CustodyUnset {
		_, _ = fmt.Fprintln(stderr, "key custody is not recorded (pre-P6-XP-04 store); run `devstrap init` to record it before relying on the unattended service")
	}
	effective := state.EffectiveKeyCustody(recorded)
	if effective == devicekeys.CustodyFile {
		return recorded != devicekeys.CustodyFile, nil
	}
	if effective != devicekeys.CustodyKeychain {
		return false, nil
	}
	if mgr.Name() == "systemd-user" && allowKeychainCustody {
		return false, nil
	}

	keychainUnreachable := devicekeys.NewHybridStore(paths.KeyDir(), keychainBackend()).
		WithCustody(effective).
		Probe(ctx) == devicekeys.CustodyFile
	unreachableNow := ""
	if keychainUnreachable {
		unreachableNow = " The keychain is unreachable even in this session."
	}

	// Service manager identity, not the build OS, describes the unit's actual
	// runtime environment and keeps the platform risk deterministic in tests.
	switch mgr.Name() {
	case "systemd-user":
		return false, appError{code: exitInvalidConfig, err: fmt.Errorf(
			"the systemd user unit runs with no session D-Bus; recorded keychain custody fails closed every tick (run-loop exits into a restart loop; a --daemon unit keeps serving but never converges).%s Re-initialize with %s=1 and migrate the key files to file custody, or pass --allow-keychain-custody if this box really has a user-session D-Bus at service runtime (for example, desktop Linux with linger)",
			unreachableNow, platform.NoKeychainEnv,
		)}
	case "launchd":
		_, _ = fmt.Fprintf(stderr, "recorded keychain custody under launchd: a locked keychain before the first GUI login after reboot makes ticks fail closed until unlock; `devstrap doctor` will name it.%s\n", unreachableNow)
	}
	return false, nil
}

// serviceUninstallResult is the --json shape for `devstrap service uninstall`
// (P5-CLI-01 part B). The stderr confirmation lines stay unchanged (P7-CLI-03,
// deliberately not gated by --quiet); this is purely additive for --json.
type serviceUninstallResult struct {
	Manager      string   `json:"manager"`
	Label        string   `json:"label"`
	WasInstalled bool     `json:"was_installed"`
	Notes        []string `json:"notes,omitempty"`
}

func newServiceUninstallCommand(stdout io.Writer, opts *options) *cobra.Command {
	var label string
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the run-loop background service",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			stderr := cmd.ErrOrStderr()
			mgr := serviceBackend()
			resolvedLabel := label
			if resolvedLabel == "" {
				resolvedLabel = mgr.DefaultLabel()
			}
			// Best-effort pre-check so we can report the idempotent "not
			// installed" case; a Status error here never blocks uninstall, but
			// it also means we cannot trust status.Installed's zero value as
			// "was not installed" — that would misreport a real removal as a
			// no-op (CodeRabbit review).
			status, statusErr := mgr.Status(cmd.Context(), resolvedLabel)
			notes, err := mgr.Uninstall(cmd.Context(), resolvedLabel)
			if err != nil {
				if errors.Is(err, platform.ErrUnsupported) {
					return appError{code: exitGeneric, err: fmt.Errorf("background service is not supported on this platform/session: %w", err)}
				}
				return err
			}
			wasInstalled := true
			switch {
			case statusErr != nil:
				// Terminal confirmation of a completed state change, deliberately not gated by --quiet (P7-CLI-03).
				_, _ = fmt.Fprintf(stderr, "uninstalled %s service %q (prior state unknown: %v)\n", mgr.Name(), resolvedLabel, statusErr)
			case status.Installed:
				// Terminal confirmation of a completed state change, deliberately not gated by --quiet (P7-CLI-03).
				_, _ = fmt.Fprintf(stderr, "uninstalled %s service %q\n", mgr.Name(), resolvedLabel)
			default:
				// Terminal confirmation of a completed state change, deliberately not gated by --quiet (P7-CLI-03).
				_, _ = fmt.Fprintf(stderr, "%s service %q not installed; nothing to do\n", mgr.Name(), resolvedLabel)
				wasInstalled = false
			}
			// Notes are operator advisories (e.g. headless systemd unit-file-only
			// removal), not mere progress — print them verbatim even under --quiet.
			for _, note := range notes {
				_, _ = fmt.Fprintln(stderr, note)
			}
			return opts.render(stdout, func(w io.Writer) error { return nil }, serviceUninstallResult{
				Manager:      mgr.Name(),
				Label:        resolvedLabel,
				WasInstalled: wasInstalled,
				Notes:        notes,
			})
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "service label (defaults to the OS-idiomatic label)")
	return cmd
}

// serviceStatusJSON is the --json shape for `service status`.
type serviceStatusJSON struct {
	Manager         string `json:"manager"`
	Label           string `json:"label"`
	Mode            string `json:"mode,omitempty"`
	Installed       bool   `json:"installed"`
	Running         bool   `json:"running"`
	Detail          string `json:"detail"`
	UnitPath        string `json:"unit_path"`
	ExecPath        string `json:"exec_path,omitempty"`
	ExecPathMissing bool   `json:"exec_path_missing,omitempty"`
}

func newServiceStatusCommand(stdout io.Writer, opts *options) *cobra.Command {
	var label string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the run-loop service status",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			mgr := serviceBackend()
			resolvedLabel := label
			if resolvedLabel == "" {
				resolvedLabel = mgr.DefaultLabel()
			}
			status, err := mgr.Status(cmd.Context(), resolvedLabel)
			if err != nil {
				if errors.Is(err, platform.ErrUnsupported) {
					return appError{code: exitGeneric, err: fmt.Errorf("background service is not supported on this platform/session: %w", err)}
				}
				return err
			}
			out := serviceStatusJSON{
				Manager:         mgr.Name(),
				Label:           resolvedLabel,
				Mode:            status.Mode,
				Installed:       status.Installed,
				Running:         status.Running,
				Detail:          status.Detail,
				UnitPath:        status.UnitPath,
				ExecPath:        status.ExecPath,
				ExecPathMissing: status.ExecPathMissing,
			}
			return opts.render(stdout, func(w io.Writer) error {
				_, _ = fmt.Fprintf(w, "manager:   %s\n", mgr.Name())
				_, _ = fmt.Fprintf(w, "label:     %s\n", resolvedLabel)
				if status.Mode != "" {
					_, _ = fmt.Fprintf(w, "mode:      %s\n", status.Mode)
				}
				_, _ = fmt.Fprintf(w, "installed: %t\n", status.Installed)
				_, _ = fmt.Fprintf(w, "running:   %t\n", status.Running)
				if status.Detail != "" {
					_, _ = fmt.Fprintf(w, "detail:    %s\n", status.Detail)
				}
				if status.UnitPath != "" {
					_, _ = fmt.Fprintf(w, "unit:      %s\n", status.UnitPath)
				}
				if status.ExecPathMissing {
					_, _ = fmt.Fprintf(w, "exec:      %s (MISSING — re-run 'devstrap service install')\n", status.ExecPath)
				} else if status.ExecPath != "" {
					_, _ = fmt.Fprintf(w, "exec:      %s\n", status.ExecPath)
				}
				return nil
			}, out)
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "service label (defaults to the OS-idiomatic label)")
	return cmd
}

// resolveServiceExecPath resolves the devstrap binary the service will run. An
// explicit --exec-path is honored verbatim but must be absolute. Otherwise the
// path comes from os.Executable() with symlinks resolved, except that a symlink
// in a stable install bin directory is preserved so Homebrew upgrades do not
// strand the service on a versioned Cellar binary (P7-XP-01). The resolved path
// is still REFUSED when it points at an ephemeral location (the OS temp dir or
// a `go build`/`go run` cache): baking such a path into a launchd/systemd unit
// would wire the service to a binary that disappears.
func resolveServiceExecPath(execPath string) (string, error) {
	if execPath != "" {
		if !filepath.IsAbs(execPath) {
			return "", appError{code: exitInvalidConfig, err: fmt.Errorf("--exec-path must be absolute, got %q", execPath)}
		}
		return execPath, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", appError{code: exitInvalidConfig, err: fmt.Errorf("resolve this binary's path: %w", err)}
	}
	return resolveServiceExecPathFrom(exe, filepath.EvalSymlinks)
}

// stableServiceBinDirs is a variable only to let tests model a stable install
// directory without writing to system-owned paths.
var stableServiceBinDirs = func() []string {
	dirs := []string{"/opt/homebrew/bin", "/usr/local/bin", "/home/linuxbrew/.linuxbrew/bin"}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".local", "bin"))
	}
	return dirs
}()

// stableBrewPrefixes back the keg-only/versioned-formula case: Homebrew's
// `<prefix>/opt/<formula>/bin` symlinks are upgrade-stable (unlike Cellar) and
// are the ONLY entrypoint for a keg-only or versioned formula, which may have
// no global bin link at all (Codex review on P7-XP-01).
var stableBrewPrefixes = []string{"/opt/homebrew", "/usr/local", "/home/linuxbrew/.linuxbrew"}

// isStableBrewOptBin reports whether dir is exactly `<brew prefix>/opt/<one
// formula segment>/bin`.
func isStableBrewOptBin(dir string) bool {
	for _, prefix := range stableBrewPrefixes {
		rel, err := filepath.Rel(prefix+"/opt", dir)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			continue
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) == 2 && parts[0] != "" && parts[1] == "bin" {
			return true
		}
	}
	return false
}

func resolveServiceExecPathFrom(exe string, evalSymlinks func(string) (string, error)) (string, error) {
	resolved, err := evalSymlinks(exe)
	if err != nil {
		return "", appError{code: exitInvalidConfig, err: fmt.Errorf("resolve this binary's path: %w", err)}
	}
	if isEphemeralExecPath(resolved) {
		return "", appError{code: exitInvalidConfig, err: fmt.Errorf(
			"this devstrap binary lives at an ephemeral path (%s); install devstrap to a stable location (e.g. /usr/local/bin) and re-run, or pass --exec-path <abs path>", resolved)}
	}
	// Preserve only known install-entry directories. The resolved target was
	// checked first so a stable-looking symlink cannot bless a temporary binary.
	if isStableBinDir(filepath.Dir(filepath.Clean(exe))) {
		return exe, nil
	}
	cellarSegment := string(os.PathSeparator) + "Cellar" + string(os.PathSeparator)
	if strings.Contains(resolved, cellarSegment) {
		return "", appError{code: exitInvalidConfig, err: fmt.Errorf(
			"the versioned Homebrew Cellar path %s would break on brew upgrade; re-run via the stable symlink (e.g. /opt/homebrew/bin/devstrap) or pass --exec-path <abs path>", resolved)}
	}
	return resolved, nil
}

func isStableBinDir(dir string) bool {
	abs, err := filepath.Abs(filepath.Clean(dir))
	if err != nil {
		return false
	}
	for _, stable := range stableServiceBinDirs {
		stableAbs, err := filepath.Abs(filepath.Clean(stable))
		if err == nil && abs == stableAbs {
			return true
		}
	}
	return isStableBrewOptBin(abs)
}

// isEphemeralExecPath reports whether p is under the OS temp dir or a Go build
// cache — the two ways os.Executable() resolves to a binary that will not
// survive (a `go run`/`go test` binary, or one unpacked to $TMPDIR).
func isEphemeralExecPath(p string) bool {
	if tmp := os.TempDir(); tmp != "" {
		// Resolve the temp dir's own symlinks (/var → /private/var on macOS) so
		// the prefix test compares real path against real path.
		if rt, err := filepath.EvalSymlinks(tmp); err == nil {
			tmp = rt
		}
		// Segment-aware prefix: filepath.Rel keeps "/tmpfoo" from matching "/tmp".
		if rel, err := filepath.Rel(tmp, p); err == nil &&
			rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return true
		}
	}
	return strings.Contains(p, "go-build")
}

// serviceConvergenceArgs bakes the argv a service unit runs. head is the
// subcommand path ("run-loop", or "daemon start"); everything after it is
// identical between the two modes, which is the point — one convergence
// contract, two supervision shapes.
//
// It bakes the interval and optional --namespace-only/--hub-file (absolute),
// and propagates the root-level --home/--root/--config ONLY when the operator
// set them explicitly, so the service inherits the same non-default state
// locations the operator is using now.
func serviceConvergenceArgs(cmd *cobra.Command, opts *options, head []string, interval time.Duration, namespaceOnly bool, hubFile string) ([]string, error) {
	args := append(append([]string{}, head...), "--interval", interval.String())
	if namespaceOnly {
		args = append(args, "--namespace-only")
	}
	if hubFile != "" {
		abs, err := filepath.Abs(hubFile)
		if err != nil {
			return nil, appError{code: exitInvalidConfig, err: fmt.Errorf("resolve --hub-file: %w", err)}
		}
		args = append(args, "--hub-file", abs)
	}
	// Absolutize each propagated path (Codex review): the service resolves
	// relative paths against launchd/systemd's working directory, not the
	// install-time cwd, so a relative --home/--root/--config would point the
	// long-lived service at the wrong state.
	root := cmd.Root().PersistentFlags()
	appendAbs := func(flag, value string) error {
		abs, err := filepath.Abs(value)
		if err != nil {
			return appError{code: exitInvalidConfig, err: fmt.Errorf("resolve --%s: %w", flag, err)}
		}
		args = append(args, "--"+flag, abs)
		return nil
	}
	if root.Changed("home") {
		if err := appendAbs("home", opts.home); err != nil {
			return nil, err
		}
	}
	if root.Changed("root") {
		if err := appendAbs("root", opts.root); err != nil {
			return nil, err
		}
	}
	if root.Changed("config") {
		if err := appendAbs("config", opts.cfgFile); err != nil {
			return nil, err
		}
	}
	return args, nil
}

func serviceRunLoopArgs(cmd *cobra.Command, opts *options, interval time.Duration, namespaceOnly bool, hubFile string) ([]string, error) {
	return serviceConvergenceArgs(cmd, opts, []string{"run-loop"}, interval, namespaceOnly, hubFile)
}

func serviceDaemonArgs(cmd *cobra.Command, opts *options, interval time.Duration, namespaceOnly bool, hubFile string) ([]string, error) {
	// Refuse up front rather than installing a unit that cannot work. A
	// daemon-mode unit supervises `daemon start`, which now REFUSES an
	// over-long socket path — so without this check the install succeeds and
	// launchd/systemd then restarts a process that fails identically every
	// time. A crash-loop discovered from logs is a far worse experience than
	// an install that says why it stopped.
	if err := opts.paths().ValidateSocketPath(); err != nil {
		return nil, appError{code: exitInvalidConfig, err: err}
	}
	return serviceConvergenceArgs(cmd, opts, []string{"daemon", "start"}, interval, namespaceOnly, hubFile)
}
