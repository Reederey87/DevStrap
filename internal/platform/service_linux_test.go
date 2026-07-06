//go:build linux

package platform

import (
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
