package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/state"
)

// fakeServiceManager is an in-memory platform.ServiceManager injected via the
// serviceBackend seam so the CLI tests never touch launchctl/systemctl.
type fakeServiceManager struct {
	nameVal        string
	labelVal       string
	installNotes   []string
	installErr     error
	uninstallNotes []string
	uninstallErr   error
	statusVal      platform.ServiceStatus
	statusErr      error

	installedSpec *platform.ServiceSpec
	uninstalled   bool
}

func (f *fakeServiceManager) Name() string {
	if f.nameVal == "" {
		return "fake"
	}
	return f.nameVal
}

func (f *fakeServiceManager) DefaultLabel() string {
	if f.labelVal == "" {
		return "fake.run-loop"
	}
	return f.labelVal
}

func (f *fakeServiceManager) Install(_ context.Context, spec platform.ServiceSpec) ([]string, error) {
	s := spec
	f.installedSpec = &s
	return f.installNotes, f.installErr
}

func (f *fakeServiceManager) Uninstall(_ context.Context, _ string) ([]string, error) {
	f.uninstalled = true
	return f.uninstallNotes, f.uninstallErr
}

func (f *fakeServiceManager) Status(_ context.Context, _ string) (platform.ServiceStatus, error) {
	return f.statusVal, f.statusErr
}

// withFakeService swaps the serviceBackend seam for the duration of the test.
func withFakeService(t *testing.T, f *fakeServiceManager) {
	t.Helper()
	prev := serviceBackend
	serviceBackend = func() platform.ServiceManager { return f }
	t.Cleanup(func() { serviceBackend = prev })
}

func serviceTestHomeWithCustody(t *testing.T, custody devicekeys.Custody) string {
	t.Helper()
	home := t.TempDir()
	store, err := state.Open(t.Context(), filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureWorkspace(t.Context(), "service-test", filepath.Join(home, "root")); err != nil {
		t.Fatal(err)
	}
	if custody != devicekeys.CustodyUnset {
		if err := store.RecordKeyCustody(t.Context(), custody); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func useServiceTestKeychain(t *testing.T, unreachable bool) {
	t.Helper()
	fake := &fakeKeychain{}
	fake.setUnreachable(unreachable)
	restore := swapKeychainBackend(func() devicekeys.SecretBackend { return fake })
	t.Cleanup(restore)
}

func TestServiceInstallBuildsRunLoopArgs(t *testing.T) {
	f := &fakeServiceManager{}
	withFakeService(t, f)
	home := t.TempDir()
	hub := filepath.Join(t.TempDir(), "hub.json")
	_, _, err := executeForTest(
		"--home", home,
		"service", "install",
		"--hub-file", hub,
		"--exec-path", "/usr/local/bin/devstrap",
		"--interval", "10m",
		"--namespace-only",
	)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if f.installedSpec == nil {
		t.Fatal("Install was not called")
	}
	want := []string{"run-loop", "--interval", "10m0s", "--namespace-only", "--hub-file", hub, "--home", home}
	if strings.Join(f.installedSpec.Args, " ") != strings.Join(want, " ") {
		t.Errorf("baked args = %v, want %v", f.installedSpec.Args, want)
	}
	if f.installedSpec.ExecPath != "/usr/local/bin/devstrap" {
		t.Errorf("exec path = %q", f.installedSpec.ExecPath)
	}
	if !f.installedSpec.RestartOnFailure || f.installedSpec.RestartDelaySeconds != 30 {
		t.Errorf("restart policy = (%v, %d), want (true, 30)", f.installedSpec.RestartOnFailure, f.installedSpec.RestartDelaySeconds)
	}
	// Log paths flow from --home through opts.paths().LogDir().
	wantOut := filepath.Join(home, "logs", "run-loop.out.log")
	if f.installedSpec.StdoutPath != wantOut {
		t.Errorf("stdout path = %q, want %q", f.installedSpec.StdoutPath, wantOut)
	}
}

func TestServiceInstallRefusesTempExecPath(t *testing.T) {
	f := &fakeServiceManager{}
	withFakeService(t, f)
	// No --exec-path: os.Executable() resolves to the go-test binary under the
	// build cache / temp dir, which must be refused.
	_, stderr, err := executeForTest("--home", t.TempDir(), "service", "install", "--hub-file", filepath.Join(t.TempDir(), "hub.json"))
	if err == nil {
		t.Fatal("expected refusal of an ephemeral exec path")
	}
	if got := ExitCodeWithWriter(err, io.Discard); got != exitInvalidConfig {
		t.Errorf("exit code = %d, want %d", got, exitInvalidConfig)
	}
	if !strings.Contains(stderr, "ephemeral path") && !strings.Contains(stderr, "stable") {
		t.Errorf("stderr = %q, want an ephemeral-path remedy", stderr)
	}
	if f.installedSpec != nil {
		t.Error("Install must not run when the exec path is refused")
	}
}

func TestResolveServiceExecPathPrefersStableSymlinkDir(t *testing.T) {
	stableDir := filepath.Join(string(os.PathSeparator), "fake-stable", "bin")
	stableExe := filepath.Join(stableDir, "devstrap")
	cellarTarget := filepath.Join(string(os.PathSeparator), "opt", "fakebrew", "Cellar", "devstrap", "1.2.3", "bin", "devstrap")
	previous := stableServiceBinDirs
	stableServiceBinDirs = []string{stableDir}
	t.Cleanup(func() { stableServiceBinDirs = previous })

	t.Run("stable symlink survives Cellar target", func(t *testing.T) {
		got, err := resolveServiceExecPathFrom(stableExe, func(string) (string, error) { return cellarTarget, nil })
		if err != nil || got != stableExe {
			t.Fatalf("resolve = (%q, %v), want stable path %q", got, err, stableExe)
		}
	})

	t.Run("keg-only opt/<formula>/bin symlink survives Cellar target", func(t *testing.T) {
		// /opt/homebrew/opt/devstrap@1/bin is the ONLY entrypoint for a
		// keg-only/versioned formula and is upgrade-stable like the bin dirs.
		for _, exe := range []string{
			"/opt/homebrew/opt/devstrap@1/bin/devstrap",
			"/usr/local/opt/devstrap@1/bin/devstrap",
			"/home/linuxbrew/.linuxbrew/opt/devstrap/bin/devstrap",
		} {
			got, err := resolveServiceExecPathFrom(exe, func(string) (string, error) { return cellarTarget, nil })
			if err != nil || got != exe {
				t.Fatalf("resolve(%q) = (%q, %v), want the opt symlink kept", exe, got, err)
			}
		}
		// A deeper or shallower opt path is NOT the keg pattern.
		for _, exe := range []string{
			"/opt/homebrew/opt/devstrap@1/libexec/bin/devstrap",
			"/opt/homebrew/opt/bin/devstrap",
		} {
			if _, err := resolveServiceExecPathFrom(exe, func(string) (string, error) { return cellarTarget, nil }); err == nil {
				t.Fatalf("resolve(%q) succeeded, want Cellar refusal", exe)
			}
		}
	})

	t.Run("direct Cellar path is refused", func(t *testing.T) {
		_, err := resolveServiceExecPathFrom("/random/bin/devstrap", func(string) (string, error) { return cellarTarget, nil })
		if err == nil || !strings.Contains(err.Error(), "brew upgrade") || !strings.Contains(err.Error(), "--exec-path") {
			t.Fatalf("error = %v, want brew-upgrade and --exec-path remedy", err)
		}
		var appErr appError
		if !errors.As(err, &appErr) || appErr.code != exitInvalidConfig {
			t.Fatalf("error = %#v, want exitInvalidConfig appError", err)
		}
	})

	t.Run("ephemeral target wins over stable directory", func(t *testing.T) {
		target := filepath.Join(os.TempDir(), "go-build123", "devstrap")
		_, err := resolveServiceExecPathFrom(stableExe, func(string) (string, error) { return target, nil })
		if err == nil || !strings.Contains(err.Error(), "ephemeral path") {
			t.Fatalf("error = %v, want ephemeral-path refusal", err)
		}
	})

	t.Run("explicit exec path remains verbatim", func(t *testing.T) {
		const absolute = "/custom/bin/devstrap"
		if got, err := resolveServiceExecPath(absolute); err != nil || got != absolute {
			t.Fatalf("absolute explicit path = (%q, %v), want %q", got, err, absolute)
		}
		if _, err := resolveServiceExecPath("relative/devstrap"); err == nil || !strings.Contains(err.Error(), "must be absolute") {
			t.Fatalf("relative explicit path error = %v, want absolute-path refusal", err)
		}
	})
}

func TestServiceInstallRequiresHubConfig(t *testing.T) {
	f := &fakeServiceManager{}
	withFakeService(t, f)
	// --home to an empty temp dir so no real config supplies a hub.
	_, stderr, err := executeForTest("--home", t.TempDir(), "service", "install", "--exec-path", "/usr/local/bin/devstrap")
	if err == nil {
		t.Fatal("expected hub-config refusal")
	}
	if got := ExitCodeWithWriter(err, io.Discard); got != exitInvalidConfig {
		t.Errorf("exit code = %d, want %d", got, exitInvalidConfig)
	}
	if !strings.Contains(stderr, "no hub configured") {
		t.Errorf("stderr = %q, want 'no hub configured'", stderr)
	}
	if f.installedSpec != nil {
		t.Error("Install must not run without a configured hub")
	}
}

func TestServiceInstallRefusesKeychainCustodyOnSystemd(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "")
	useServiceTestKeychain(t, false)
	f := &fakeServiceManager{nameVal: "systemd-user"}
	withFakeService(t, f)
	home := serviceTestHomeWithCustody(t, devicekeys.CustodyKeychain)
	_, stderr, err := executeForTest("--home", home, "service", "install", "--hub-file", filepath.Join(t.TempDir(), "hub.json"), "--exec-path", "/usr/local/bin/devstrap")
	if err == nil {
		t.Fatal("expected keychain-custody refusal")
	}
	if got := ExitCodeWithWriter(err, io.Discard); got != exitInvalidConfig {
		t.Errorf("exit code = %d, want %d", got, exitInvalidConfig)
	}
	if !strings.Contains(stderr, platform.NoKeychainEnv) || !strings.Contains(stderr, "--allow-keychain-custody") {
		t.Errorf("stderr = %q, want file-custody and explicit-override remedies", stderr)
	}
	if f.installedSpec != nil {
		t.Error("Install must not run for systemd keychain custody without opt-in")
	}
}

func TestServiceInstallAllowsKeychainCustodyWithExplicitFlag(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "")
	restore := swapKeychainBackend(func() devicekeys.SecretBackend {
		panic("explicit systemd opt-in must not probe the keychain")
	})
	defer restore()
	f := &fakeServiceManager{nameVal: "systemd-user"}
	withFakeService(t, f)
	home := serviceTestHomeWithCustody(t, devicekeys.CustodyKeychain)
	_, _, err := executeForTest("--home", home, "service", "install", "--hub-file", filepath.Join(t.TempDir(), "hub.json"), "--exec-path", "/usr/local/bin/devstrap", "--allow-keychain-custody")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if f.installedSpec == nil {
		t.Fatal("Install was not called")
	}
}

func TestServiceInstallWarnsKeychainCustodyOnLaunchd(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "")
	useServiceTestKeychain(t, false)
	f := &fakeServiceManager{nameVal: "launchd"}
	withFakeService(t, f)
	home := serviceTestHomeWithCustody(t, devicekeys.CustodyKeychain)
	_, stderr, err := executeForTest("--home", home, "--quiet", "service", "install", "--hub-file", filepath.Join(t.TempDir(), "hub.json"), "--exec-path", "/usr/local/bin/devstrap")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !strings.Contains(stderr, "locked keychain") {
		t.Errorf("stderr = %q, want locked-keychain warning even under --quiet", stderr)
	}
	if f.installedSpec == nil {
		t.Fatal("Install was not called")
	}
}

func TestServiceInstallReportsCurrentlyUnreachableKeychain(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "")
	useServiceTestKeychain(t, true)
	f := &fakeServiceManager{nameVal: "launchd"}
	withFakeService(t, f)
	home := serviceTestHomeWithCustody(t, devicekeys.CustodyKeychain)
	_, stderr, err := executeForTest("--home", home, "service", "install", "--hub-file", filepath.Join(t.TempDir(), "hub.json"), "--exec-path", "/usr/local/bin/devstrap")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !strings.Contains(stderr, "keychain is unreachable even in this session") {
		t.Errorf("stderr = %q, want current-session probe detail", stderr)
	}
}

func TestServiceInstallFileCustodySilent(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "")
	f := &fakeServiceManager{nameVal: "systemd-user"}
	withFakeService(t, f)
	home := serviceTestHomeWithCustody(t, devicekeys.CustodyFile)
	_, stderr, err := executeForTest("--home", home, "--quiet", "service", "install", "--hub-file", filepath.Join(t.TempDir(), "hub.json"), "--exec-path", "/usr/local/bin/devstrap")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if strings.Contains(stderr, "custody") || strings.Contains(stderr, "keychain") {
		t.Errorf("stderr = %q, want no custody output", stderr)
	}
}

func TestServiceInstallCarriesEffectiveFileCustodyIntoUnit(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	f := &fakeServiceManager{nameVal: "systemd-user"}
	withFakeService(t, f)
	home := serviceTestHomeWithCustody(t, devicekeys.CustodyKeychain)
	_, stderr, err := executeForTest("--home", home, "service", "install", "--hub-file", filepath.Join(t.TempDir(), "hub.json"), "--exec-path", "/usr/local/bin/devstrap")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if f.installedSpec == nil || f.installedSpec.Env[platform.NoKeychainEnv] != "1" {
		t.Fatalf("installed spec = %+v, want %s=1 carried into the service", f.installedSpec, platform.NoKeychainEnv)
	}
	if strings.Contains(stderr, "custody") || strings.Contains(stderr, "keychain") {
		t.Errorf("stderr = %q, want effective file custody to proceed silently", stderr)
	}
}

func TestServiceInstallUnsetCustodyWarns(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "")
	f := &fakeServiceManager{nameVal: "systemd-user"}
	withFakeService(t, f)
	home := serviceTestHomeWithCustody(t, devicekeys.CustodyUnset)
	_, stderr, err := executeForTest("--home", home, "--quiet", "service", "install", "--hub-file", filepath.Join(t.TempDir(), "hub.json"), "--exec-path", "/usr/local/bin/devstrap")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !strings.Contains(stderr, "devstrap init") {
		t.Errorf("stderr = %q, want init warning even under --quiet", stderr)
	}
}

func TestServiceInstallPreservesBehaviorBeforeInit(t *testing.T) {
	f := &fakeServiceManager{nameVal: "systemd-user"}
	withFakeService(t, f)
	home := t.TempDir()
	store, err := state.Open(t.Context(), filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	closeStore(store)

	_, _, err = executeForTest("--home", home, "service", "install", "--hub-file", filepath.Join(t.TempDir(), "hub.json"), "--exec-path", "/usr/local/bin/devstrap")
	if err != nil {
		t.Fatalf("install before init: %v", err)
	}
	if f.installedSpec == nil {
		t.Fatal("Install was not called")
	}
}

func TestServiceInstallPreInitStillBakesExplicitOverride(t *testing.T) {
	// A pre-init install with DEVSTRAP_NO_KEYCHAIN=1 set must carry the
	// override into the unit: init may later record keychain custody, and the
	// service must keep behaving like the session that installed it.
	t.Setenv(platform.NoKeychainEnv, "1")
	f := &fakeServiceManager{nameVal: "systemd-user"}
	withFakeService(t, f)
	_, _, err := executeForTest("--home", t.TempDir(), "service", "install", "--hub-file", filepath.Join(t.TempDir(), "hub.json"), "--exec-path", "/usr/local/bin/devstrap")
	if err != nil {
		t.Fatalf("install before init: %v", err)
	}
	if f.installedSpec == nil || f.installedSpec.Env[platform.NoKeychainEnv] != "1" {
		t.Fatalf("installed spec = %+v, want %s=1 baked pre-init", f.installedSpec, platform.NoKeychainEnv)
	}
}

func TestServiceInstallUnsetCustodyWarnsEvenWithOverride(t *testing.T) {
	// DEVSTRAP_NO_KEYCHAIN=1 makes an unset store effectively file-backed, but
	// the pre-P6-XP-04 warning (and the bake) must both still happen.
	t.Setenv(platform.NoKeychainEnv, "1")
	f := &fakeServiceManager{nameVal: "systemd-user"}
	withFakeService(t, f)
	home := serviceTestHomeWithCustody(t, devicekeys.CustodyUnset)
	_, stderr, err := executeForTest("--home", home, "--quiet", "service", "install", "--hub-file", filepath.Join(t.TempDir(), "hub.json"), "--exec-path", "/usr/local/bin/devstrap")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !strings.Contains(stderr, "devstrap init") {
		t.Errorf("stderr = %q, want the init warning to survive the override", stderr)
	}
	if f.installedSpec == nil || f.installedSpec.Env[platform.NoKeychainEnv] != "1" {
		t.Fatalf("installed spec = %+v, want the override baked alongside the warning", f.installedSpec)
	}
}

func TestServiceInstallRefusesCorruptRecordedCustody(t *testing.T) {
	// A recorded custody value outside file/keychain is corrupt state:
	// HybridStore would treat it as keychain-preferred-but-not-required and
	// silently re-enable the file fallback the custody model forbids.
	t.Setenv(platform.NoKeychainEnv, "")
	f := &fakeServiceManager{nameVal: "systemd-user"}
	withFakeService(t, f)
	home := serviceTestHomeWithCustody(t, devicekeys.CustodyKeychain)
	db, err := sql.Open("sqlite", filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE local_meta SET value = 'sideways' WHERE key = 'key_custody'`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	_, _, err = executeForTest("--home", home, "service", "install", "--hub-file", filepath.Join(t.TempDir(), "hub.json"), "--exec-path", "/usr/local/bin/devstrap")
	if err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("install err = %v, want corrupt-custody refusal", err)
	}
	var appErr appError
	if !errors.As(err, &appErr) || appErr.code != exitInvalidConfig {
		t.Fatalf("error = %#v, want exitInvalidConfig", err)
	}
	if f.installedSpec != nil {
		t.Fatal("Install must not run with corrupt recorded custody")
	}
}

func TestServiceInstallPrintsAdapterNotes(t *testing.T) {
	f := &fakeServiceManager{installNotes: []string{"systemd lingering is off; run: loginctl enable-linger $USER"}}
	withFakeService(t, f)
	_, stderr, err := executeForTest("--home", t.TempDir(), "service", "install", "--hub-file", filepath.Join(t.TempDir(), "hub.json"), "--exec-path", "/usr/local/bin/devstrap")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !strings.Contains(stderr, "enable-linger") {
		t.Errorf("stderr = %q, want the adapter note printed verbatim", stderr)
	}
	if !strings.Contains(stderr, "installed fake service") {
		t.Errorf("stderr = %q, want the install progress line", stderr)
	}
}

func TestServiceInstallConfirmationSurvivesQuiet(t *testing.T) {
	// P7-CLI-03: the terminal "installed … service" line must print even under
	// --quiet; auxiliary progress (logs:) stays gated.
	f := &fakeServiceManager{}
	withFakeService(t, f)
	_, stderr, err := executeForTest(
		"--home", t.TempDir(),
		"--quiet",
		"service", "install",
		"--hub-file", filepath.Join(t.TempDir(), "hub.json"),
		"--exec-path", "/usr/local/bin/devstrap",
	)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !strings.Contains(stderr, "installed fake service") {
		t.Errorf("stderr = %q, want install confirmation even under --quiet", stderr)
	}
	if strings.Contains(stderr, "logs:") {
		t.Errorf("stderr = %q, want logs: progress suppressed under --quiet", stderr)
	}
}

func TestServiceUninstallConfirmationSurvivesQuiet(t *testing.T) {
	// P7-CLI-03: uninstall / not-installed terminal confirmations survive --quiet.
	f := &fakeServiceManager{statusVal: platform.ServiceStatus{Installed: true}}
	withFakeService(t, f)
	_, stderr, err := executeForTest("--home", t.TempDir(), "--quiet", "service", "uninstall")
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !strings.Contains(stderr, "uninstalled fake service") {
		t.Errorf("stderr = %q, want uninstall confirmation even under --quiet", stderr)
	}

	f2 := &fakeServiceManager{statusVal: platform.ServiceStatus{Installed: false}}
	withFakeService(t, f2)
	_, stderr2, err := executeForTest("--home", t.TempDir(), "--quiet", "service", "uninstall")
	if err != nil {
		t.Fatalf("uninstall (not installed): %v", err)
	}
	if !strings.Contains(stderr2, "not installed") {
		t.Errorf("stderr = %q, want not-installed confirmation even under --quiet", stderr2)
	}
}

func TestServiceInstallEnvContainsNoSecrets(t *testing.T) {
	// Pin the override off: CI exports DEVSTRAP_NO_KEYCHAIN=1 job-wide, and an
	// explicitly-set override is DELIBERATELY baked (the one non-secret env the
	// CLI supplies); this test asserts the no-override default of nil env.
	t.Setenv(platform.NoKeychainEnv, "")
	f := &fakeServiceManager{}
	withFakeService(t, f)
	_, _, err := executeForTest("--home", t.TempDir(), "service", "install", "--hub-file", filepath.Join(t.TempDir(), "hub.json"), "--exec-path", "/usr/local/bin/devstrap")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if f.installedSpec.Env != nil {
		t.Errorf("spec.Env = %v, want nil (without the explicit override the CLI bakes no env; adapters add only PATH)", f.installedSpec.Env)
	}
}

func TestServiceUninstallPrintsAdapterNotesEvenQuiet(t *testing.T) {
	f := &fakeServiceManager{
		statusVal:      platform.ServiceStatus{Installed: true},
		uninstallNotes: []string{"systemd user manager unreachable; removed the unit file only"},
	}
	withFakeService(t, f)
	_, stderr, err := executeForTest("--home", t.TempDir(), "--quiet", "service", "uninstall")
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !strings.Contains(stderr, "removed the unit file only") {
		t.Errorf("stderr = %q, want the adapter note printed verbatim even under --quiet", stderr)
	}
}

func TestServiceUninstallIdempotent(t *testing.T) {
	// Not installed: uninstall is a success no-op with a note.
	f := &fakeServiceManager{statusVal: platform.ServiceStatus{Installed: false}}
	withFakeService(t, f)
	_, stderr, err := executeForTest("--home", t.TempDir(), "service", "uninstall")
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !f.uninstalled {
		t.Error("Uninstall was not called")
	}
	if !strings.Contains(stderr, "not installed") {
		t.Errorf("stderr = %q, want a not-installed note", stderr)
	}

	// Installed: uninstall reports removal.
	f2 := &fakeServiceManager{statusVal: platform.ServiceStatus{Installed: true}}
	withFakeService(t, f2)
	_, stderr2, err := executeForTest("--home", t.TempDir(), "service", "uninstall")
	if err != nil {
		t.Fatalf("uninstall (installed): %v", err)
	}
	if !strings.Contains(stderr2, "uninstalled fake service") {
		t.Errorf("stderr = %q, want an uninstalled line", stderr2)
	}
}

// TestServiceUninstallStatusErrorDoesNotClaimNotInstalled is the CodeRabbit
// review guard (PR #184): a failed pre-check Status call must not be treated
// as proof the service was not installed. Before the fix, a Status error left
// status.Installed at its false zero value, so a real removal (Uninstall
// succeeds with no error) was misreported as "not installed; nothing to do".
func TestServiceUninstallStatusErrorDoesNotClaimNotInstalled(t *testing.T) {
	f := &fakeServiceManager{statusErr: errors.New("transient launchctl print failure")}
	withFakeService(t, f)
	_, stderr, err := executeForTest("--home", t.TempDir(), "service", "uninstall")
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !f.uninstalled {
		t.Error("Uninstall was not called")
	}
	if strings.Contains(stderr, "not installed; nothing to do") {
		t.Errorf("stderr = %q, must not claim not-installed on an unknown prior state", stderr)
	}
	if !strings.Contains(stderr, "uninstalled fake service") || !strings.Contains(stderr, "prior state unknown") {
		t.Errorf("stderr = %q, want an uninstalled confirmation noting the unknown prior state", stderr)
	}
}

func TestServiceStatusJSON(t *testing.T) {
	f := &fakeServiceManager{statusVal: platform.ServiceStatus{Installed: true, Running: false, Detail: "not loaded", UnitPath: "/x/fake.plist", ExecPath: "/x/devstrap", ExecPathMissing: true}}
	withFakeService(t, f)
	stdout, _, err := executeForTest("--home", t.TempDir(), "--json", "service", "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var got serviceStatusJSON
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", stdout, err)
	}
	want := serviceStatusJSON{Manager: "fake", Label: "fake.run-loop", Installed: true, Running: false, Detail: "not loaded", UnitPath: "/x/fake.plist", ExecPath: "/x/devstrap", ExecPathMissing: true}
	if got != want {
		t.Errorf("status json = %+v, want %+v", got, want)
	}
}

func TestServiceStatusHumanReportsMissingExecPath(t *testing.T) {
	f := &fakeServiceManager{statusVal: platform.ServiceStatus{Installed: true, UnitPath: "/x/fake.plist", ExecPath: "/x/devstrap", ExecPathMissing: true}}
	withFakeService(t, f)
	stdout, _, err := executeForTest("--home", t.TempDir(), "service", "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(stdout, "exec:      /x/devstrap (MISSING — re-run 'devstrap service install')") {
		t.Errorf("stdout = %q, want missing ExecPath remedy", stdout)
	}
}

func TestServiceUnsupportedExitsNonzero(t *testing.T) {
	f := &fakeServiceManager{installErr: fmt.Errorf("%w: no launchd domain", platform.ErrUnsupported)}
	withFakeService(t, f)
	_, stderr, err := executeForTest("--home", t.TempDir(), "service", "install", "--hub-file", filepath.Join(t.TempDir(), "hub.json"), "--exec-path", "/usr/local/bin/devstrap")
	if err == nil {
		t.Fatal("expected an error for an unsupported platform")
	}
	if got := ExitCodeWithWriter(err, io.Discard); got != exitGeneric {
		t.Errorf("exit code = %d, want %d", got, exitGeneric)
	}
	if !strings.Contains(stderr, "not supported on this platform") {
		t.Errorf("stderr = %q, want an unsupported-platform message", stderr)
	}
}
