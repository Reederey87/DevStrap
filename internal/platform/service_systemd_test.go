package platform

import (
	"testing"
)

func TestRenderSystemdUnitGolden(t *testing.T) {
	got, err := renderSystemdUnit(goldenServiceSpec())
	if err != nil {
		t.Fatalf("renderSystemdUnit: %v", err)
	}
	checkGolden(t, "run_loop.service.golden", got)
}

func TestSystemdQuoteEscapesSpacesAndPercent(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"run-loop", "run-loop"},
		{"/usr/local/bin/devstrap", "/usr/local/bin/devstrap"},
		{"a b", `"a b"`},
		{"100%", "100%%"},
		{"a b%c", `"a b%%c"`},
		{`quote"d`, `"quote\"d"`},
		{`back\slash space`, `"back\\slash space"`},
		{"", `""`},
	}
	for _, c := range cases {
		if got := systemdQuote(c.in); got != c.want {
			t.Errorf("systemdQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSystemdQuoteUnquoteFirstWordRoundTrip(t *testing.T) {
	for _, word := range []string{"plain", "with spaces", `quote"inside`, `back\slash space`, "100% ready"} {
		got, ok := systemdUnquoteFirstWord(systemdQuote(word) + " trailing-arg")
		if !ok || got != word {
			t.Errorf("round trip %q = (%q, %t)", word, got, ok)
		}
	}
}

func TestExtractSystemdExecPath(t *testing.T) {
	unit, err := renderSystemdUnit(ServiceSpec{
		Label:    "devstrap-run-loop",
		ExecPath: "/opt/bin/devstrap with space",
		Args:     []string{"run-loop", "--interval", "5m"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := extractSystemdExecPath(unit); got != "/opt/bin/devstrap with space" {
		t.Errorf("extractSystemdExecPath = %q, want the rendered ExecPath", got)
	}
	// A hand-mangled unit degrades to an unknown ExecPath, never a panic.
	if got := extractSystemdExecPath([]byte("ExecStart=\"mangled\\q\"\n")); got != "" {
		t.Errorf("extractSystemdExecPath(mangled) = %q, want empty", got)
	}
	if got := extractSystemdExecPath([]byte("[Unit]\nDescription=x\n")); got != "" {
		t.Errorf("extractSystemdExecPath(no ExecStart) = %q, want empty", got)
	}
}

func TestSystemdArgvBuilders(t *testing.T) {
	if got := systemdProbeArgs(); !equalStrings(got, []string{"systemctl", "--user", "show-environment"}) {
		t.Errorf("probe argv = %v", got)
	}
	if got := systemdReloadArgs(); !equalStrings(got, []string{"systemctl", "--user", "daemon-reload"}) {
		t.Errorf("reload argv = %v", got)
	}
	if got := systemdEnableArgs("devstrap-run-loop.service"); !equalStrings(got, []string{"systemctl", "--user", "enable", "devstrap-run-loop.service"}) {
		t.Errorf("enable argv = %v", got)
	}
	if got := systemdRestartArgs("devstrap-run-loop.service"); !equalStrings(got, []string{"systemctl", "--user", "restart", "devstrap-run-loop.service"}) {
		t.Errorf("restart argv = %v", got)
	}
	if got := systemdDisableNowArgs("devstrap-run-loop.service"); !equalStrings(got, []string{"systemctl", "--user", "disable", "--now", "devstrap-run-loop.service"}) {
		t.Errorf("disable-now argv = %v", got)
	}
	if got := systemdIsActiveArgs("devstrap-run-loop.service"); !equalStrings(got, []string{"systemctl", "--user", "is-active", "devstrap-run-loop.service"}) {
		t.Errorf("is-active argv = %v", got)
	}
	if got := lingerProbeArgs("dev"); !equalStrings(got, []string{"loginctl", "show-user", "dev", "--property=Linger", "--value"}) {
		t.Errorf("linger argv = %v", got)
	}
	if got := systemdUnitPath("/units", "devstrap-run-loop"); got != "/units/devstrap-run-loop.service" {
		t.Errorf("unit path = %q", got)
	}
	if got := defaultLinuxPath("/opt/devstrap/bin", "/home/dev"); got != "/opt/devstrap/bin:/home/dev/.local/bin:/usr/local/bin:/usr/bin:/bin" {
		t.Errorf("defaultLinuxPath = %q", got)
	}
}

// TestRenderersRejectControlCharacters (Codex review, HIGH): unit files are
// line-oriented, so a newline in any interpolated value injects directives;
// XML 1.0 forbids raw control bytes. Both renderers fail closed.
func TestRenderersRejectControlCharacters(t *testing.T) {
	base := ServiceSpec{Label: "devstrap-run-loop", ExecPath: "/usr/local/bin/devstrap"}
	mutations := []func(*ServiceSpec){
		func(s *ServiceSpec) { s.Args = []string{"run-loop", "--hub-file", "/tmp/x\nExecStartPre=/bin/evil"} },
		func(s *ServiceSpec) { s.ExecPath = "/usr/local/bin/dev\nstrap" },
		func(s *ServiceSpec) { s.Description = "desc\nExecStart=/bin/evil" },
		func(s *ServiceSpec) { s.WorkingDir = "/tmp\n" },
		func(s *ServiceSpec) { s.Env = map[string]string{"PATH": "/bin\nEnvironment=X=y"} },
		func(s *ServiceSpec) { s.Env = map[string]string{"PA\rTH": "/bin"} },
		func(s *ServiceSpec) { s.StdoutPath = "/tmp/out\x00log" },
	}
	for i, mutate := range mutations {
		spec := base
		mutate(&spec)
		if _, err := renderSystemdUnit(spec); err == nil {
			t.Fatalf("mutation %d: renderSystemdUnit accepted a control character", i)
		}
		if _, err := renderLaunchdPlist(spec); err == nil {
			t.Fatalf("mutation %d: renderLaunchdPlist accepted a control character", i)
		}
	}
	if _, err := renderSystemdUnit(base); err != nil {
		t.Fatalf("clean spec must render: %v", err)
	}
	if _, err := renderLaunchdPlist(base); err != nil {
		t.Fatalf("clean spec must render: %v", err)
	}
}
