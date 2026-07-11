package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goldenServiceSpec is the canonical spec both the launchd and systemd golden
// tests render, so the two golden files describe the same logical service.
func goldenServiceSpec() ServiceSpec {
	return ServiceSpec{
		Label:               "com.devstrap.run-loop",
		Description:         "DevStrap run-loop (scan + sync + materialize)",
		ExecPath:            "/usr/local/bin/devstrap",
		Args:                []string{"run-loop", "--interval", "5m0s", "--hub-file", "/home/dev/hub.json"},
		Env:                 map[string]string{"PATH": "/usr/local/bin:/usr/bin:/bin"},
		WorkingDir:          "/home/dev/Code",
		StdoutPath:          "/home/dev/.devstrap/logs/run-loop.out.log",
		StderrPath:          "/home/dev/.devstrap/logs/run-loop.err.log",
		RestartOnFailure:    true,
		RestartDelaySeconds: 30,
	}
}

// checkGolden compares got against the golden file, rewriting it when
// UPDATE_GOLDEN=1 so the fixtures can be regenerated deliberately.
func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v", path, err)
	}
	if string(got) != string(want) {
		t.Errorf("%s mismatch:\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func TestRenderLaunchdPlistGolden(t *testing.T) {
	got, err := renderLaunchdPlist(goldenServiceSpec())
	if err != nil {
		t.Fatalf("renderLaunchdPlist: %v", err)
	}
	checkGolden(t, "run_loop.plist.golden", got)
}

func TestExtractLaunchdExecPathGolden(t *testing.T) {
	plist, err := os.ReadFile(filepath.Join("testdata", "run_loop.plist.golden"))
	if err != nil {
		t.Fatal(err)
	}
	if got := extractLaunchdExecPath(plist); got != "/usr/local/bin/devstrap" {
		t.Errorf("extractLaunchdExecPath = %q, want /usr/local/bin/devstrap", got)
	}
}

func TestRenderLaunchdPlistEscapesXML(t *testing.T) {
	spec := ServiceSpec{
		Label:    "com.devstrap.run-loop",
		ExecPath: "/usr/local/bin/devstrap",
		Args:     []string{"run-loop", `A & B < C > "D"`},
		Env:      map[string]string{"WEIRD": `x & y < z > "q"`},
	}
	got, err := renderLaunchdPlist(spec)
	if err != nil {
		t.Fatalf("renderLaunchdPlist: %v", err)
	}
	out := string(got)
	// The raw metacharacters must never appear inside a rendered <string>.
	for _, bad := range []string{"& B", "< C", `"D"`, "& y"} {
		if strings.Contains(out, bad) {
			t.Errorf("unescaped metacharacters %q present in plist:\n%s", bad, out)
		}
	}
	for _, want := range []string{"&amp;", "&lt;", "&gt;", "&#34;"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected escaped entity %q missing from plist:\n%s", want, out)
		}
	}
}

func TestRenderLaunchdPlistOmitsOptionalKeys(t *testing.T) {
	// A minimal spec: no restart, no logs, no working dir, no env.
	spec := ServiceSpec{
		Label:    "com.devstrap.run-loop",
		ExecPath: "/usr/local/bin/devstrap",
		Args:     []string{"run-loop"},
	}
	got, err := renderLaunchdPlist(spec)
	if err != nil {
		t.Fatalf("renderLaunchdPlist: %v", err)
	}
	out := string(got)
	for _, key := range []string{"KeepAlive", "StandardOutPath", "StandardErrorPath", "WorkingDirectory", "EnvironmentVariables"} {
		if strings.Contains(out, key) {
			t.Errorf("optional key %q should be omitted for a minimal spec:\n%s", key, out)
		}
	}
	// RunAtLoad and the default ThrottleInterval are always present.
	if !strings.Contains(out, "<key>RunAtLoad</key>") {
		t.Errorf("RunAtLoad missing:\n%s", out)
	}
	if !strings.Contains(out, "<key>ThrottleInterval</key>\n\t<integer>30</integer>") {
		t.Errorf("default ThrottleInterval 30 missing:\n%s", out)
	}
}

func TestLaunchdArgvBuilders(t *testing.T) {
	if got := launchdBootstrapArgs(501, "/x/foo.plist"); !equalStrings(got, []string{"launchctl", "bootstrap", "gui/501", "/x/foo.plist"}) {
		t.Errorf("bootstrap argv = %v", got)
	}
	if got := launchdBootoutArgs(501, "com.devstrap.run-loop"); !equalStrings(got, []string{"launchctl", "bootout", "gui/501/com.devstrap.run-loop"}) {
		t.Errorf("bootout argv = %v", got)
	}
	if got := launchdPrintArgs(501, "com.devstrap.run-loop"); !equalStrings(got, []string{"launchctl", "print", "gui/501/com.devstrap.run-loop"}) {
		t.Errorf("print argv = %v", got)
	}
	if got := launchdPlistPath("/agents", "com.devstrap.run-loop"); got != "/agents/com.devstrap.run-loop.plist" {
		t.Errorf("plist path = %q", got)
	}
	if got := defaultDarwinPath("/opt/devstrap/bin"); got != "/opt/devstrap/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin" {
		t.Errorf("defaultDarwinPath = %q", got)
	}
}

func TestParseLaunchctlPrint(t *testing.T) {
	cases := []struct {
		name        string
		out         string
		wantRunning bool
		wantDetail  string
	}{
		{
			name:        "running",
			out:         "com.devstrap.run-loop = {\n\tstate = running\n\tpid = 4242\n}",
			wantRunning: true,
			wantDetail:  "state running, pid 4242",
		},
		{
			name:        "loaded not running",
			out:         "com.devstrap.run-loop = {\n\tstate = not running\n}",
			wantRunning: false,
			wantDetail:  "state not running",
		},
		{
			name:        "last exit code",
			out:         "com.devstrap.run-loop = {\n\tstate = not running\n\tlast exit code = 78\n}",
			wantRunning: false,
			wantDetail:  "state not running, last exit code 78",
		},
		{
			// Real `launchctl print` shape (from live dogfood): the service's own
			// top-level `state = running` precedes nested per-endpoint `state =
			// active` lines. The service state must win — a last-match parse
			// misreported this live service as not running.
			name:        "nested endpoint states do not shadow service state",
			out:         "com.devstrap.run-loop = {\n\tstate = running\n\tpid = 30206\n\tlast exit code = (never exited)\n\tendpoints = {\n\t\t\"com.devstrap\" = {\n\t\t\tstate = active\n\t\t}\n\t}\n}",
			wantRunning: true,
			wantDetail:  "state running, pid 30206, last exit code (never exited)",
		},
		{
			name:        "garbage degrades without error",
			out:         "totally unexpected blob\nwith no known fields",
			wantRunning: false,
			wantDetail:  "loaded (state unknown)",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			running, detail := parseLaunchctlPrint(c.out)
			if running != c.wantRunning || detail != c.wantDetail {
				t.Errorf("parseLaunchctlPrint(%q) = (%v, %q), want (%v, %q)", c.name, running, detail, c.wantRunning, c.wantDetail)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestValidateServiceLabel (coordinator review): the label reaches
// filepath.Join, a launchctl domain target, and a systemd unit name, so
// separators, traversal, and whitespace must be refused before any path or
// argv is built from it.
func TestValidateServiceLabel(t *testing.T) {
	for _, ok := range []string{"com.devstrap.run-loop", "devstrap-run-loop", "a", "A9._-x"} {
		if err := validateServiceLabel(ok); err != nil {
			t.Fatalf("validateServiceLabel(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "../../evil", "a/b", "a b", ".hidden", "-lead", "a\x00b", "a\nb"} {
		if err := validateServiceLabel(bad); err == nil {
			t.Fatalf("validateServiceLabel(%q) = nil, want error", bad)
		}
	}
}
