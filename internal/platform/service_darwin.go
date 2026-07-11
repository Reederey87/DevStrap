//go:build darwin

package platform

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// LaunchdManager installs the run-loop as a per-user launchd LaunchAgent via
// the modern `launchctl bootstrap`/`bootout`/`print` verbs. Zero-value fields
// resolve to ~/Library/LaunchAgents and the current uid.
type LaunchdManager struct {
	AgentsDir string
	UID       int
}

func (m LaunchdManager) Name() string { return "launchd" }

func (m LaunchdManager) DefaultLabel() string { return "com.devstrap.run-loop" }

func (m LaunchdManager) agentsDir() (string, error) {
	if m.AgentsDir != "" {
		return m.AgentsDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents"), nil
}

func (m LaunchdManager) uid() int {
	if m.UID != 0 {
		return m.UID
	}
	return os.Getuid()
}

func (m LaunchdManager) Install(ctx context.Context, spec ServiceSpec) ([]string, error) {
	if err := validateServiceLabel(spec.Label); err != nil {
		return nil, fmt.Errorf("launchd: %w", err)
	}
	if !filepath.IsAbs(spec.ExecPath) {
		return nil, fmt.Errorf("launchd: exec path must be absolute, got %q", spec.ExecPath)
	}
	agentsDir, err := m.agentsDir()
	if err != nil {
		return nil, err
	}
	// Seed a PATH so a Homebrew/user install of the binary and its sibling
	// tools (git, gh) resolve when launchd runs the service with a bare env.
	if spec.Env == nil {
		spec.Env = map[string]string{}
	}
	if spec.Env["PATH"] == "" {
		spec.Env["PATH"] = defaultDarwinPath(filepath.Dir(spec.ExecPath))
	}
	plist, err := renderLaunchdPlist(spec)
	if err != nil {
		return nil, err
	}
	//nolint:gosec // ~/Library/LaunchAgents is a standard user dir launchd must traverse; it holds only the 0600 plist, no secret material.
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		return nil, fmt.Errorf("create LaunchAgents dir: %w", err)
	}
	for _, logPath := range []string{spec.StdoutPath, spec.StderrPath} {
		if logPath == "" {
			continue
		}
		//nolint:gosec // Standard log dir under DevStrap home; the log files themselves are 0600.
		if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
			return nil, fmt.Errorf("create log dir: %w", err)
		}
	}
	plistPath := launchdPlistPath(agentsDir, spec.Label)
	if err := atomicWrite(plistPath, plist, 0o600); err != nil {
		return nil, err
	}
	// Best-effort bootout first so a reinstall is idempotent: booting out an
	// already-loaded service lets the following bootstrap succeed. A not-loaded
	// service makes bootout fail, which is expected and ignored.
	uid := m.uid()
	_, _ = runLaunchctl(ctx, launchdBootoutArgs(uid, spec.Label))
	// bootout is asynchronous: tearing down a *running* service's process lags,
	// and bootstrapping into a domain that still holds the old job fails with
	// EIO ("Bootstrap failed: 5: Input/output error" — caught in live dogfood).
	// Wait until the label is gone from the domain before bootstrapping.
	m.waitUntilBootedOut(ctx, uid, spec.Label)
	if stderr, err := runLaunchctl(ctx, launchdBootstrapArgs(uid, plistPath)); err != nil {
		return nil, fmt.Errorf("launchctl bootstrap: %w: %s", err, stderr)
	}
	return nil, nil
}

// waitUntilBootedOut polls `launchctl print` until the label is no longer in
// the domain (print exits non-zero), so a reinstall's bootstrap does not race
// the previous job's asynchronous teardown. Bounded and best-effort: on a
// not-loaded service it returns on the first poll, and on timeout it returns so
// the caller bootstraps anyway (surfacing any real error there).
func (m LaunchdManager) waitUntilBootedOut(ctx context.Context, uid int, label string) {
	for i := 0; i < 30; i++ {
		if _, err := runLaunchctlOut(ctx, launchdPrintArgs(uid, label)); err != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (m LaunchdManager) Uninstall(ctx context.Context, label string) ([]string, error) {
	if err := validateServiceLabel(label); err != nil {
		return nil, fmt.Errorf("launchd: %w", err)
	}
	agentsDir, err := m.agentsDir()
	if err != nil {
		return nil, err
	}
	// bootout failure means the service was not loaded — idempotent uninstall.
	_, _ = runLaunchctl(ctx, launchdBootoutArgs(m.uid(), label))
	if err := os.Remove(launchdPlistPath(agentsDir, label)); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove plist: %w", err)
	}
	return nil, nil
}

func (m LaunchdManager) Status(ctx context.Context, label string) (ServiceStatus, error) {
	if err := validateServiceLabel(label); err != nil {
		return ServiceStatus{}, fmt.Errorf("launchd: %w", err)
	}
	agentsDir, err := m.agentsDir()
	if err != nil {
		return ServiceStatus{}, err
	}
	plistPath := launchdPlistPath(agentsDir, label)
	status := ServiceStatus{UnitPath: plistPath}
	if _, err := os.Stat(plistPath); err != nil {
		if os.IsNotExist(err) {
			status.Detail = "not installed"
			return status, nil
		}
		return status, fmt.Errorf("stat plist: %w", err)
	}
	status.Installed = true
	out, err := runLaunchctlOut(ctx, launchdPrintArgs(m.uid(), label))
	if err != nil {
		// A non-zero print exit means the service is not loaded in the domain
		// (installed on disk but not bootstrapped).
		status.Running = false
		status.Detail = "not loaded"
		return status, nil
	}
	status.Running, status.Detail = parseLaunchctlPrint(out)
	return status, nil
}

// runLaunchctl runs a launchctl argv (built by the argv builders) and returns
// its captured stderr. launchctl is resolved from PATH so tests can shim it.
func runLaunchctl(ctx context.Context, argv []string) (string, error) {
	var stderr bytes.Buffer
	//nolint:gosec // argv comes from the fixed launchctl argv builders (verb + domain/label), not user input.
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stderr.String(), err
}

// runLaunchctlOut runs a launchctl argv and returns its captured stdout.
func runLaunchctlOut(ctx context.Context, argv []string) (string, error) {
	var stdout bytes.Buffer
	//nolint:gosec // argv comes from the fixed launchctl argv builders (verb + domain/label), not user input.
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout = &stdout
	err := cmd.Run()
	return stdout.String(), err
}
