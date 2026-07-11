package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/platform"
)

// fakeServiceManager is an in-memory platform.ServiceManager injected via the
// serviceBackend seam so the CLI tests never touch launchctl/systemctl.
type fakeServiceManager struct {
	nameVal      string
	labelVal     string
	installNotes []string
	installErr   error
	uninstallErr error
	statusVal    platform.ServiceStatus
	statusErr    error

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
	return nil, f.uninstallErr
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

func TestServiceInstallEnvContainsNoSecrets(t *testing.T) {
	f := &fakeServiceManager{}
	withFakeService(t, f)
	_, _, err := executeForTest("--home", t.TempDir(), "service", "install", "--hub-file", filepath.Join(t.TempDir(), "hub.json"), "--exec-path", "/usr/local/bin/devstrap")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if f.installedSpec.Env != nil {
		t.Errorf("spec.Env = %v, want nil (the CLI bakes no env; adapters add only PATH)", f.installedSpec.Env)
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

func TestServiceStatusJSON(t *testing.T) {
	f := &fakeServiceManager{statusVal: platform.ServiceStatus{Installed: true, Running: false, Detail: "not loaded", UnitPath: "/x/fake.plist"}}
	withFakeService(t, f)
	stdout, _, err := executeForTest("--home", t.TempDir(), "--json", "service", "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var got serviceStatusJSON
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", stdout, err)
	}
	want := serviceStatusJSON{Manager: "fake", Label: "fake.run-loop", Installed: true, Running: false, Detail: "not loaded", UnitPath: "/x/fake.plist"}
	if got != want {
		t.Errorf("status json = %+v, want %+v", got, want)
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
