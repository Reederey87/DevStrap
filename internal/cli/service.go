package cli

import (
	"context"
	"encoding/json"
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

// newServiceCommand implements `devstrap service install|uninstall|status`
// (P4-PROD-04): it wraps the existing `run-loop` in a per-user launchd
// LaunchAgent (macOS) or systemd user service (Linux) so the workspace
// converges unattended, without a bespoke daemon.
func newServiceCommand(stdout io.Writer, opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Install the run-loop as a background OS service",
	}
	cmd.AddCommand(newServiceInstallCommand(stdout, opts))
	cmd.AddCommand(newServiceUninstallCommand(stdout, opts))
	cmd.AddCommand(newServiceStatusCommand(stdout, opts))
	return cmd
}

func newServiceInstallCommand(stdout io.Writer, opts *options) *cobra.Command {
	var interval time.Duration
	var namespaceOnly bool
	var hubFile string
	var label string
	var execPath string
	var allowKeychainCustody bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install and start the run-loop background service",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			stderr := cmd.ErrOrStderr()
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
			bakedArgs, err := serviceRunLoopArgs(cmd, opts, interval, namespaceOnly, hubFile)
			if err != nil {
				return err
			}
			resolvedLabel := label
			if resolvedLabel == "" {
				resolvedLabel = mgr.DefaultLabel()
			}
			var serviceEnv map[string]string
			if forceFileCustody {
				serviceEnv = map[string]string{platform.NoKeychainEnv: "1"}
			}
			logDir := opts.paths().LogDir()
			spec := platform.ServiceSpec{
				Label:       resolvedLabel,
				Description: "DevStrap run-loop (scan + sync + materialize)",
				ExecPath:    resolvedExec,
				Args:        bakedArgs,
				StdoutPath:  filepath.Join(logDir, "run-loop.out.log"),
				StderrPath:  filepath.Join(logDir, "run-loop.err.log"),
				// Coupled to run-loop's own consecutive-failure ceiling — see the
				// note by runLoopMaxConsecutiveFailures. Env stays nil unless the
				// explicit non-secret file-custody override must survive into the
				// service; adapters add PATH, and no secret enters a service file.
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
			opts.progressf(stderr, "installed %s service %q\n", mgr.Name(), resolvedLabel)
			if status, serr := mgr.Status(cmd.Context(), resolvedLabel); serr == nil && status.UnitPath != "" {
				opts.progressf(stderr, "unit: %s\n", status.UnitPath)
			}
			opts.progressf(stderr, "logs: %s, %s\n", spec.StdoutPath, spec.StderrPath)
			// Notes are operator advisories (e.g. the Linux linger caveat), not
			// mere progress — print them verbatim even under --quiet.
			for _, note := range notes {
				_, _ = fmt.Fprintln(stderr, note)
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&interval, "interval", 5*time.Minute, "run-loop sync interval")
	cmd.Flags().BoolVar(&namespaceOnly, "namespace-only", false, "sync namespace metadata only; skip materialization")
	cmd.Flags().StringVar(&hubFile, "hub-file", "", "file-backed test hub path")
	cmd.Flags().StringVar(&label, "label", "", "service label (defaults to the OS-idiomatic label)")
	cmd.Flags().StringVar(&execPath, "exec-path", "", "absolute path to the devstrap binary the service runs (defaults to this binary)")
	cmd.Flags().BoolVar(&allowKeychainCustody, "allow-keychain-custody", false, "allow a systemd user service to use recorded keychain custody")
	return cmd
}

func checkServiceInstallCustody(ctx context.Context, stderr io.Writer, opts *options, mgr platform.ServiceManager, allowKeychainCustody bool) (bool, error) {
	paths := opts.paths()
	if _, err := os.Stat(paths.StateDB()); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Preserve the pre-init install behavior; hub/exec validation remains
			// authoritative when no state store exists yet.
			return false, nil
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
			return false, nil
		}
		return false, err
	}
	recorded, err := store.KeyCustody(ctx)
	if err != nil {
		return false, err
	}
	effective := state.EffectiveKeyCustody(recorded)
	if effective == devicekeys.CustodyFile {
		return recorded != devicekeys.CustodyFile, nil
	}
	if recorded == devicekeys.CustodyUnset {
		_, _ = fmt.Fprintln(stderr, "key custody is not recorded (pre-P6-XP-04 store); run `devstrap init` to record it before relying on the unattended service")
		return false, nil
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
			"the systemd user unit runs with no session D-Bus; recorded keychain custody fails closed every tick, and the run-loop will exit into a restart loop.%s Re-initialize with %s=1 and migrate the key files to file custody, or pass --allow-keychain-custody if this box really has a user-session D-Bus at service runtime (for example, desktop Linux with linger)",
			unreachableNow, platform.NoKeychainEnv,
		)}
	case "launchd":
		_, _ = fmt.Fprintf(stderr, "recorded keychain custody under launchd: a locked keychain before the first GUI login after reboot makes ticks fail closed until unlock; `devstrap doctor` will name it.%s\n", unreachableNow)
	}
	return false, nil
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
			// installed" case; a Status error here never blocks uninstall.
			status, _ := mgr.Status(cmd.Context(), resolvedLabel)
			notes, err := mgr.Uninstall(cmd.Context(), resolvedLabel)
			if err != nil {
				if errors.Is(err, platform.ErrUnsupported) {
					return appError{code: exitGeneric, err: fmt.Errorf("background service is not supported on this platform/session: %w", err)}
				}
				return err
			}
			if status.Installed {
				opts.progressf(stderr, "uninstalled %s service %q\n", mgr.Name(), resolvedLabel)
			} else {
				opts.progressf(stderr, "%s service %q not installed; nothing to do\n", mgr.Name(), resolvedLabel)
			}
			// Notes are operator advisories (e.g. headless systemd unit-file-only
			// removal), not mere progress — print them verbatim even under --quiet.
			for _, note := range notes {
				_, _ = fmt.Fprintln(stderr, note)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "service label (defaults to the OS-idiomatic label)")
	return cmd
}

// serviceStatusJSON is the --json shape for `service status`.
type serviceStatusJSON struct {
	Manager         string `json:"manager"`
	Label           string `json:"label"`
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
			if opts.v.GetBool("json") {
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(serviceStatusJSON{
					Manager:         mgr.Name(),
					Label:           resolvedLabel,
					Installed:       status.Installed,
					Running:         status.Running,
					Detail:          status.Detail,
					UnitPath:        status.UnitPath,
					ExecPath:        status.ExecPath,
					ExecPathMissing: status.ExecPathMissing,
				})
			}
			_, _ = fmt.Fprintf(stdout, "manager:   %s\n", mgr.Name())
			_, _ = fmt.Fprintf(stdout, "label:     %s\n", resolvedLabel)
			_, _ = fmt.Fprintf(stdout, "installed: %t\n", status.Installed)
			_, _ = fmt.Fprintf(stdout, "running:   %t\n", status.Running)
			if status.Detail != "" {
				_, _ = fmt.Fprintf(stdout, "detail:    %s\n", status.Detail)
			}
			if status.UnitPath != "" {
				_, _ = fmt.Fprintf(stdout, "unit:      %s\n", status.UnitPath)
			}
			if status.ExecPathMissing {
				_, _ = fmt.Fprintf(stdout, "exec:      %s (MISSING — re-run 'devstrap service install')\n", status.ExecPath)
			} else if status.ExecPath != "" {
				_, _ = fmt.Fprintf(stdout, "exec:      %s\n", status.ExecPath)
			}
			return nil
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

// serviceRunLoopArgs builds the run-loop argv the service executes. It bakes
// the interval and optional --namespace-only/--hub-file (absolute), and
// propagates the root-level --home/--root/--config ONLY when the operator set
// them explicitly, so the service inherits the same non-default state locations
// the operator is using now.
func serviceRunLoopArgs(cmd *cobra.Command, opts *options, interval time.Duration, namespaceOnly bool, hubFile string) ([]string, error) {
	args := []string{"run-loop", "--interval", interval.String()}
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
