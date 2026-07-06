//go:build darwin

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

func TestLaunchdManagerInstallBootsOutThenBootstraps(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("SHIM_LOG", logPath)
	// print exits non-zero (service not loaded) so the post-bootout drain poll
	// returns on its first probe. The bootstrap branch records whether the plist
	// ($3) already exists, proving the plist was written before bootstrap ran.
	stubExec(t, "launchctl", `echo "$@" >> "$SHIM_LOG"
case "$1" in
  print) exit 1 ;;
  bootstrap)
    if [ -f "$3" ]; then echo "plist-present" >> "$SHIM_LOG"; else echo "plist-absent" >> "$SHIM_LOG"; fi ;;
esac
exit 0`)

	agentsDir := t.TempDir()
	mgr := LaunchdManager{AgentsDir: agentsDir, UID: 501}
	spec := ServiceSpec{
		Label:    "com.devstrap.run-loop",
		ExecPath: "/usr/local/bin/devstrap",
		Args:     []string{"run-loop"},
	}
	notes, err := mgr.Install(t.Context(), spec)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if notes != nil {
		t.Errorf("notes = %v, want nil", notes)
	}
	plistPath := launchdPlistPath(agentsDir, spec.Label)
	if _, err := os.Stat(plistPath); err != nil {
		t.Fatalf("plist not written: %v", err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read shim log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	bootoutIdx := indexWithPrefix(lines, "bootout ")
	bootstrapIdx := indexWithPrefix(lines, "bootstrap ")
	if bootoutIdx < 0 || bootstrapIdx < 0 {
		t.Fatalf("expected both bootout and bootstrap calls, got %v", lines)
	}
	if bootoutIdx > bootstrapIdx {
		t.Errorf("bootout must precede bootstrap (reinstall idempotency); calls = %v", lines)
	}
	if !strings.Contains(string(raw), "plist-present") {
		t.Errorf("plist was not present when bootstrap ran:\n%s", raw)
	}
}

func indexWithPrefix(lines []string, prefix string) int {
	for i, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return i
		}
	}
	return -1
}

func TestLaunchdManagerUninstallIdempotent(t *testing.T) {
	// bootout exits 3 (service not loaded) — Uninstall must ignore it.
	stubExec(t, "launchctl", "exit 3")
	agentsDir := t.TempDir()
	mgr := LaunchdManager{AgentsDir: agentsDir, UID: 501}
	plistPath := launchdPlistPath(agentsDir, "com.devstrap.run-loop")
	if err := os.WriteFile(plistPath, []byte("<plist/>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Uninstall(t.Context(), "com.devstrap.run-loop"); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(plistPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("plist still present after uninstall: %v", err)
	}
	// A second uninstall (plist already gone) is still a no-error no-op.
	if err := mgr.Uninstall(t.Context(), "com.devstrap.run-loop"); err != nil {
		t.Fatalf("second Uninstall: %v", err)
	}
}

func TestLaunchdManagerStatusParsesPrint(t *testing.T) {
	stubExec(t, "launchctl", `if [ "$1" = "print" ]; then
  printf 'com.devstrap.run-loop = {\n\tstate = running\n\tpid = 4242\n}\n'
  exit 0
fi
exit 0`)
	agentsDir := t.TempDir()
	mgr := LaunchdManager{AgentsDir: agentsDir, UID: 501}
	plistPath := launchdPlistPath(agentsDir, "com.devstrap.run-loop")
	if err := os.WriteFile(plistPath, []byte("<plist/>"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := mgr.Status(t.Context(), "com.devstrap.run-loop")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.Installed || !status.Running {
		t.Errorf("status = %+v, want installed+running", status)
	}
	if status.UnitPath != plistPath {
		t.Errorf("UnitPath = %q, want %q", status.UnitPath, plistPath)
	}
	if !strings.Contains(status.Detail, "pid 4242") {
		t.Errorf("Detail = %q, want it to mention pid 4242", status.Detail)
	}
}
