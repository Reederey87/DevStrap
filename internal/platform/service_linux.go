//go:build linux

package platform

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
)

// SystemdUserManager installs the run-loop as a systemd --user service. A
// zero-value UnitDir resolves to $XDG_CONFIG_HOME/systemd/user (or
// ~/.config/systemd/user).
type SystemdUserManager struct {
	UnitDir string
}

func (m SystemdUserManager) Name() string { return "systemd-user" }

func (m SystemdUserManager) DefaultLabel() string { return "devstrap-run-loop" }

func (m SystemdUserManager) unitDir() (string, error) {
	if m.UnitDir != "" {
		return m.UnitDir, nil
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "systemd", "user"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user"), nil
}

// available gates every mutating operation: a missing systemctl or an
// unreachable --user manager (no user session / no D-Bus, the headless/CI case)
// is classified as ErrUnsupported so the CLI can degrade cleanly rather than
// emit a confusing systemctl error.
func (m SystemdUserManager) available(ctx context.Context) error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return fmt.Errorf("%w: systemd user manager unavailable (no systemd, or no user session/D-Bus): %w", ErrUnsupported, err)
	}
	if _, _, err := runSystemctl(ctx, systemdProbeArgs()); err != nil {
		return fmt.Errorf("%w: systemd user manager unavailable (no systemd, or no user session/D-Bus): %w", ErrUnsupported, err)
	}
	return nil
}

func (m SystemdUserManager) Install(ctx context.Context, spec ServiceSpec) ([]string, error) {
	if err := validateServiceLabel(spec.Label); err != nil {
		return nil, fmt.Errorf("systemd: %w", err)
	}
	if !filepath.IsAbs(spec.ExecPath) {
		return nil, fmt.Errorf("systemd: exec path must be absolute, got %q", spec.ExecPath)
	}
	if err := m.available(ctx); err != nil {
		return nil, err
	}
	if spec.Env == nil {
		spec.Env = map[string]string{}
	}
	if spec.Env["PATH"] == "" {
		home, _ := os.UserHomeDir()
		spec.Env["PATH"] = defaultLinuxPath(filepath.Dir(spec.ExecPath), home)
	}
	unit, err := renderSystemdUnit(spec)
	if err != nil {
		return nil, err
	}
	unitDir, err := m.unitDir()
	if err != nil {
		return nil, err
	}
	//nolint:gosec // ~/.config/systemd/user is a standard user dir systemd must traverse; it holds only the 0600 unit file, no secret material.
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return nil, fmt.Errorf("create systemd user dir: %w", err)
	}
	unitPath := systemdUnitPath(unitDir, spec.Label)
	if err := atomicWrite(unitPath, unit, 0o600); err != nil {
		return nil, err
	}
	if _, stderr, err := runSystemctl(ctx, systemdReloadArgs()); err != nil {
		return nil, fmt.Errorf("systemctl daemon-reload: %w: %s", err, stderr)
	}
	unitName := spec.Label + ".service"
	if _, stderr, err := runSystemctl(ctx, systemdEnableArgs(unitName)); err != nil {
		return nil, fmt.Errorf("systemctl enable: %w: %s", err, stderr)
	}
	if _, stderr, err := runSystemctl(ctx, systemdRestartArgs(unitName)); err != nil {
		return nil, fmt.Errorf("systemctl restart: %w: %s", err, stderr)
	}
	return m.lingerNotes(ctx), nil
}

// lingerNotes returns the advisory note when systemd lingering is off (or the
// probe cannot determine it): without linger the user service stops when the
// last session ends, so a headless/boot deployment needs enable-linger. The
// probe never fails the install.
func (m SystemdUserManager) lingerNotes(ctx context.Context) []string {
	if lingerEnabled(ctx) {
		return nil
	}
	return []string{"systemd lingering is off for this user; the service stops when your last session ends. For headless/boot persistence run: loginctl enable-linger $USER (may require sudo)"}
}

func lingerEnabled(ctx context.Context) bool {
	username := currentUsername()
	if username == "" {
		return false
	}
	stdout, _, err := runSystemctl(ctx, lingerProbeArgs(username))
	if err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(stdout), "yes")
}

func currentUsername() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}

func (m SystemdUserManager) Uninstall(ctx context.Context, label string) ([]string, error) {
	if err := validateServiceLabel(label); err != nil {
		return nil, fmt.Errorf("systemd: %w", err)
	}
	// Probe availability but never bail: a unit installed from a desktop session
	// must still be removable over SSH/headless (no user D-Bus). Match launchd —
	// best-effort supervisor teardown, then unconditional unit-file removal.
	managerErr := m.available(ctx)
	// A canceled/expired context must abort here: available() would classify it
	// as "manager unreachable" and the headless path would proceed to removal —
	// a canceled uninstall must not delete anything (Codex review).
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	unitName := label + ".service"
	var teardownNote string
	if managerErr == nil {
		// disable --now usually fails only for the benign not-enabled/not-loaded
		// case (idempotent). Distinguish a REAL teardown failure by probing
		// is-active: only a service that is provably still running earns an
		// advisory — the unit file is removed either way (Codex review).
		if _, _, err := runSystemctl(ctx, systemdDisableNowArgs(unitName)); err != nil {
			if _, _, activeErr := runSystemctl(ctx, systemdIsActiveArgs(unitName)); activeErr == nil {
				teardownNote = "systemctl --user disable --now " + unitName + " failed and the service is still active; the unit file was removed anyway — inspect with: systemctl --user status " + unitName
			}
		}
	}
	unitDir, err := m.unitDir()
	if err != nil {
		return nil, err
	}
	removeErr := os.Remove(systemdUnitPath(unitDir, label))
	if removeErr != nil && !os.IsNotExist(removeErr) {
		return nil, fmt.Errorf("remove unit file: %w", removeErr)
	}
	if managerErr == nil {
		if _, stderr, err := runSystemctl(ctx, systemdReloadArgs()); err != nil {
			return nil, fmt.Errorf("systemctl daemon-reload: %w: %s", err, stderr)
		}
		if teardownNote != "" {
			return []string{teardownNote}, nil
		}
		return nil, nil
	}
	if removeErr != nil {
		// Nothing was installed and nothing was removed: a headless uninstall
		// of a never-installed service stays a clean, note-free no-op.
		return nil, nil
	}
	return []string{
		"systemd user manager unreachable; removed the unit file only. If the service is still running (lingering session), run from a user session: systemctl --user disable --now " + unitName + " && systemctl --user daemon-reload",
	}, nil
}

func (m SystemdUserManager) Status(ctx context.Context, label string) (ServiceStatus, error) {
	if err := validateServiceLabel(label); err != nil {
		return ServiceStatus{}, fmt.Errorf("systemd: %w", err)
	}
	unitDir, err := m.unitDir()
	if err != nil {
		return ServiceStatus{}, err
	}
	unitPath := systemdUnitPath(unitDir, label)
	unitName := label + ".service"
	status := ServiceStatus{UnitPath: unitPath}
	// Installed is a pure stat so status works even when the --user manager is
	// unreachable (headless): the unit file is the durable install artifact.
	if _, err := os.Stat(unitPath); err != nil {
		if os.IsNotExist(err) {
			status.Detail = "not installed"
			return status, nil
		}
		return status, fmt.Errorf("stat unit file: %w", err)
	}
	status.Installed = true
	//nolint:gosec // unitPath is our own systemd user-unit for a validated label (validateServiceLabel), not user input.
	if unit, err := os.ReadFile(unitPath); err == nil {
		status.ExecPath = extractSystemdExecPath(unit)
		if status.ExecPath != "" {
			if _, err := os.Stat(status.ExecPath); os.IsNotExist(err) {
				status.ExecPathMissing = true
			}
		}
	}
	logHint := "logs: journalctl --user -u " + unitName
	if err := m.available(ctx); err != nil {
		status.Detail = "installed; systemd user manager unreachable (run from a user session to query run state); " + logHint
		prependMissingExecPathDetail(&status)
		return status, nil
	}
	stdout, _, err := runSystemctl(ctx, systemdIsActiveArgs(unitName))
	status.Running = err == nil
	state := strings.TrimSpace(stdout)
	if state == "" {
		state = "unknown"
	}
	status.Detail = state + "; " + logHint
	prependMissingExecPathDetail(&status)
	return status, nil
}

// runSystemctl runs a systemctl/loginctl argv (from the argv builders) and
// returns its stdout and stderr. The binary is resolved from PATH so tests can
// shim it.
func runSystemctl(ctx context.Context, argv []string) (stdout, stderr string, err error) {
	var outBuf, errBuf bytes.Buffer
	//nolint:gosec // argv comes from the fixed systemctl/loginctl argv builders (verb + unit/user), not user input.
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return outBuf.String(), errBuf.String(), err
}
