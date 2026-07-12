//go:build linux

package platform

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubExec installs a fake `name` binary on PATH whose behavior is the given
// shell script body (the P6-QUAL-04 PATH-shim pattern, cloned from
// internal/cli/hub_test.go's stubOp). Returns the temp dir.
func stubExec(t *testing.T, name, script string) string {
	t.Helper()
	dir := t.TempDir()
	shim := filepath.Join(dir, name)
	if err := os.WriteFile(shim, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestSystemdManagerUnavailableIsErrUnsupported(t *testing.T) {
	// An empty PATH means systemctl cannot be found — the no-systemd case.
	t.Setenv("PATH", t.TempDir())
	mgr := SystemdUserManager{UnitDir: t.TempDir()}
	spec := ServiceSpec{Label: "devstrap-run-loop", ExecPath: "/usr/local/bin/devstrap", Args: []string{"run-loop"}}
	if _, err := mgr.Install(t.Context(), spec); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Install err = %v, want ErrUnsupported", err)
	}
}

func TestSystemdManagerInstallSequenceAndLingerNote(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("SHIM_LOG", logPath)
	stubExec(t, "systemctl", `echo "$@" >> "$SHIM_LOG"
exit 0`)
	// loginctl reports lingering off; it does not write to the systemctl log so
	// the ordering assertions stay clean.
	stubExec(t, "loginctl", `echo no`)

	unitDir := t.TempDir()
	mgr := SystemdUserManager{UnitDir: unitDir}
	spec := ServiceSpec{Label: "devstrap-run-loop", ExecPath: "/usr/local/bin/devstrap", Args: []string{"run-loop"}}
	notes, err := mgr.Install(t.Context(), spec)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	unitPath := systemdUnitPath(unitDir, spec.Label)
	if _, err := os.Stat(unitPath); err != nil {
		t.Fatalf("unit file not written: %v", err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read shim log: %v", err)
	}
	got := strings.Join(strings.Split(strings.TrimSpace(string(raw)), "\n"), " | ")
	// available() probe → daemon-reload → enable → restart, in that order.
	want := "--user show-environment | --user daemon-reload | --user enable devstrap-run-loop.service | --user restart devstrap-run-loop.service"
	if got != want {
		t.Errorf("systemctl call sequence:\n got:  %s\n want: %s", got, want)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "enable-linger") {
		t.Errorf("notes = %v, want the linger advisory", notes)
	}
}

func TestSystemdManagerStatusIsActive(t *testing.T) {
	stubExec(t, "systemctl", `if [ "$2" = "is-active" ]; then echo active; fi
exit 0`)
	unitDir := t.TempDir()
	mgr := SystemdUserManager{UnitDir: unitDir}
	unitPath := systemdUnitPath(unitDir, "devstrap-run-loop")
	if err := os.WriteFile(unitPath, []byte("[Unit]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := mgr.Status(t.Context(), "devstrap-run-loop")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.Installed || !status.Running {
		t.Errorf("status = %+v, want installed+running", status)
	}
	if status.UnitPath != unitPath {
		t.Errorf("UnitPath = %q, want %q", status.UnitPath, unitPath)
	}
	if !strings.Contains(status.Detail, "active") || !strings.Contains(status.Detail, "journalctl --user -u devstrap-run-loop.service") {
		t.Errorf("Detail = %q, want active + journalctl hint", status.Detail)
	}
}

func assertSystemdHeadlessUninstall(t *testing.T, mgr SystemdUserManager, unitDir, label string) {
	t.Helper()
	unitPath := systemdUnitPath(unitDir, label)
	notes, err := mgr.Uninstall(t.Context(), label)
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(unitPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unit file still present after uninstall: %v", err)
	}
	if len(notes) == 0 {
		t.Fatal("notes empty, want headless advisory")
	}
	if !strings.Contains(notes[0], "systemctl --user disable --now") {
		t.Errorf("notes = %v, want disable --now advisory", notes)
	}
	status, err := mgr.Status(t.Context(), label)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Installed {
		t.Errorf("Installed = true after uninstall, want false")
	}
	if status.Detail != "not installed" {
		t.Errorf("Detail = %q, want %q", status.Detail, "not installed")
	}
}

func TestSystemdUninstallRemovesUnitFileWhenManagerUnreachable(t *testing.T) {
	// Empty PATH: systemctl cannot be found — headless / no-systemd case.
	t.Setenv("PATH", t.TempDir())
	unitDir := t.TempDir()
	label := "devstrap-run-loop"
	mgr := SystemdUserManager{UnitDir: unitDir}
	unitPath := systemdUnitPath(unitDir, label)
	if err := os.WriteFile(unitPath, []byte("[Unit]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertSystemdHeadlessUninstall(t, mgr, unitDir, label)
}

func TestSystemdUninstallDeadBusStillRemovesUnit(t *testing.T) {
	// systemctl is present but every verb fails — present-but-no-D-Bus case
	// (available() probe fails).
	stubExec(t, "systemctl", "exit 1")
	unitDir := t.TempDir()
	label := "devstrap-run-loop"
	mgr := SystemdUserManager{UnitDir: unitDir}
	unitPath := systemdUnitPath(unitDir, label)
	if err := os.WriteFile(unitPath, []byte("[Unit]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertSystemdHeadlessUninstall(t, mgr, unitDir, label)
}

func TestSystemdUninstallHeadlessNotInstalledIsNoteFreeNoOp(t *testing.T) {
	// No unit file and no reachable manager: nothing was removed, so the
	// "removed the unit file only" advisory would be misleading — expect a
	// clean, note-free success.
	t.Setenv("PATH", t.TempDir())
	mgr := SystemdUserManager{UnitDir: t.TempDir()}
	notes, err := mgr.Uninstall(t.Context(), "devstrap-run-loop")
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if len(notes) != 0 {
		t.Errorf("notes = %v, want none when nothing was removed", notes)
	}
}

func TestSystemdUninstallCanceledContextRemovesNothing(t *testing.T) {
	// A canceled context must abort before any removal: available() would
	// misread it as "manager unreachable" and the headless path would delete
	// the unit file of an uninstall the caller already gave up on.
	t.Setenv("PATH", t.TempDir())
	unitDir := t.TempDir()
	label := "devstrap-run-loop"
	mgr := SystemdUserManager{UnitDir: unitDir}
	unitPath := systemdUnitPath(unitDir, label)
	if err := os.WriteFile(unitPath, []byte("[Unit]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := mgr.Uninstall(ctx, label); !errors.Is(err, context.Canceled) {
		t.Fatalf("Uninstall err = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(unitPath); err != nil {
		t.Errorf("unit file was touched by a canceled uninstall: %v", err)
	}
}

func TestSystemdUninstallStillActiveAfterDisableFailureGetsAdvisory(t *testing.T) {
	// disable --now fails but is-active says the service is still running: the
	// unit file is removed, and the caller gets an advisory naming the live
	// service instead of a silent "uninstalled".
	stubExec(t, "systemctl", `case "$2" in
	disable) exit 1 ;;
	is-active) echo active; exit 0 ;;
	*) exit 0 ;;
	esac`)
	unitDir := t.TempDir()
	label := "devstrap-run-loop"
	mgr := SystemdUserManager{UnitDir: unitDir}
	unitPath := systemdUnitPath(unitDir, label)
	if err := os.WriteFile(unitPath, []byte("[Unit]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	notes, err := mgr.Uninstall(t.Context(), label)
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(unitPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unit file still present after uninstall: %v", err)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "still active") {
		t.Errorf("notes = %v, want still-active advisory", notes)
	}
}

func TestSystemdUninstallReachableManagerKeepsFullSequence(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("SHIM_LOG", logPath)
	stubExec(t, "systemctl", `echo "$@" >> "$SHIM_LOG"
exit 0`)

	unitDir := t.TempDir()
	label := "devstrap-run-loop"
	mgr := SystemdUserManager{UnitDir: unitDir}
	unitPath := systemdUnitPath(unitDir, label)
	if err := os.WriteFile(unitPath, []byte("[Unit]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	notes, err := mgr.Uninstall(t.Context(), label)
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if len(notes) != 0 {
		t.Errorf("notes = %v, want empty when manager reachable", notes)
	}
	if _, err := os.Stat(unitPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unit file still present after uninstall: %v", err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read shim log: %v", err)
	}
	got := string(raw)
	if !strings.Contains(got, "disable --now "+label+".service") {
		t.Errorf("shim log missing disable --now:\n%s", got)
	}
	if !strings.Contains(got, "daemon-reload") {
		t.Errorf("shim log missing daemon-reload:\n%s", got)
	}
}

func TestSystemdUninstallDaemonReloadFailureKeepsRemovalContext(t *testing.T) {
	stubExec(t, "systemctl", `if [ "$2" = "daemon-reload" ]; then echo reload-broke >&2; exit 1; fi
exit 0`)
	unitDir := t.TempDir()
	label := "devstrap-run-loop"
	unitPath := systemdUnitPath(unitDir, label)
	if err := os.WriteFile(unitPath, []byte("[Unit]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := (SystemdUserManager{UnitDir: unitDir}).Uninstall(t.Context(), label)
	if err == nil || !strings.Contains(err.Error(), "unit file removed, but systemctl daemon-reload failed") || !strings.Contains(err.Error(), "reload-broke") {
		t.Fatalf("Uninstall error = %v, want removal context and daemon-reload stderr", err)
	}
	if _, statErr := os.Stat(unitPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("unit file still present after uninstall: %v", statErr)
	}
}

func TestServiceStatusReportsMissingExecPath(t *testing.T) {
	stubExec(t, "systemctl", `if [ "$2" = "is-active" ]; then echo inactive; exit 3; fi
exit 0`)
	stubExec(t, "loginctl", `echo yes`)
	unitDir := t.TempDir()
	execPath := filepath.Join(t.TempDir(), "devstrap with space")
	if err := os.WriteFile(execPath, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	mgr := SystemdUserManager{UnitDir: unitDir}
	spec := ServiceSpec{Label: "devstrap-run-loop", ExecPath: execPath, Args: []string{"run-loop"}}
	if _, err := mgr.Install(t.Context(), spec); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := os.Remove(execPath); err != nil {
		t.Fatal(err)
	}
	status, err := mgr.Status(t.Context(), spec.Label)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.ExecPath != execPath || !status.ExecPathMissing || !strings.Contains(status.Detail, "ExecPath missing") {
		t.Errorf("status = %+v, want missing ExecPath %q", status, execPath)
	}

	if err := os.WriteFile(systemdUnitPath(unitDir, spec.Label), []byte("ExecStart=\"mangled\\q\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err = mgr.Status(t.Context(), spec.Label)
	if err != nil {
		t.Fatalf("Status(mangled): %v", err)
	}
	if status.ExecPath != "" || status.ExecPathMissing {
		t.Errorf("mangled status = %+v, want empty parsed ExecPath", status)
	}
}
