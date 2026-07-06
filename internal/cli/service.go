package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Reederey87/DevStrap/internal/platform"
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
			resolvedExec, err := resolveServiceExecPath(execPath)
			if err != nil {
				return err
			}
			bakedArgs, err := serviceRunLoopArgs(cmd, opts, interval, namespaceOnly, hubFile)
			if err != nil {
				return err
			}
			mgr := serviceBackend()
			resolvedLabel := label
			if resolvedLabel == "" {
				resolvedLabel = mgr.DefaultLabel()
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
				// note by runLoopMaxConsecutiveFailures. Env stays nil: the
				// adapters add only PATH, and no secret ever enters a service file.
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
	return cmd
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
			if err := mgr.Uninstall(cmd.Context(), resolvedLabel); err != nil {
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
			return nil
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "service label (defaults to the OS-idiomatic label)")
	return cmd
}

// serviceStatusJSON is the --json shape for `service status`.
type serviceStatusJSON struct {
	Manager   string `json:"manager"`
	Label     string `json:"label"`
	Installed bool   `json:"installed"`
	Running   bool   `json:"running"`
	Detail    string `json:"detail"`
	UnitPath  string `json:"unit_path"`
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
					Manager:   mgr.Name(),
					Label:     resolvedLabel,
					Installed: status.Installed,
					Running:   status.Running,
					Detail:    status.Detail,
					UnitPath:  status.UnitPath,
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
			return nil
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "service label (defaults to the OS-idiomatic label)")
	return cmd
}

// resolveServiceExecPath resolves the devstrap binary the service will run. An
// explicit --exec-path is honored verbatim but must be absolute. Otherwise the
// path comes from os.Executable() with symlinks resolved, and is REFUSED when
// it points at an ephemeral location (the OS temp dir or a `go build`/`go run`
// cache): baking such a path into a launchd/systemd unit would wire the service
// to a binary that disappears.
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
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", appError{code: exitInvalidConfig, err: fmt.Errorf("resolve this binary's path: %w", err)}
	}
	if isEphemeralExecPath(resolved) {
		return "", appError{code: exitInvalidConfig, err: fmt.Errorf(
			"this devstrap binary lives at an ephemeral path (%s); install devstrap to a stable location (e.g. /usr/local/bin) and re-run, or pass --exec-path <abs path>", resolved)}
	}
	return resolved, nil
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
