package shellhook

import (
	"strings"
	"testing"
)

func TestScriptKnownShells(t *testing.T) {
	cases := []struct {
		shell   string
		wantSub string
	}{
		// Bash must append to the PROMPT_COMMAND array, never assign it
		// (assignment would clobber whatever direnv/Starship/etc already put
		// there — the exact breakage class this package exists to avoid).
		{"bash", "PROMPT_COMMAND+=(_devstrap_prompt_precmd)"},
		{"zsh", "precmd_functions+=(_devstrap_prompt_precmd)"},
		{"fish", "--on-event fish_prompt"},
	}
	for _, c := range cases {
		got, err := Script(c.shell)
		if err != nil {
			t.Fatalf("Script(%q) unexpected error: %v", c.shell, err)
		}
		if !strings.Contains(got, c.wantSub) {
			t.Errorf("Script(%q) = %q, want substring %q", c.shell, got, c.wantSub)
		}
		if !strings.Contains(got, "devstrap status --prompt") {
			t.Errorf("Script(%q) does not invoke `devstrap status --prompt`:\n%s", c.shell, got)
		}
	}
}

func TestScriptUnknownShell(t *testing.T) {
	if _, err := Script("tcsh"); err == nil {
		t.Fatal("Script(\"tcsh\") expected an error, got nil")
	}
}

func TestShells(t *testing.T) {
	got := Shells()
	want := []string{"bash", "zsh", "fish"}
	if len(got) != len(want) {
		t.Fatalf("Shells() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Shells()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// bash/zsh must never assign PROMPT_COMMAND/precmd_functions with `=`
// (only `+=`) — a bare `=` assignment is exactly the clobbering bug this
// package guards against.
func TestScriptNeverAssignsWithBareEquals(t *testing.T) {
	for _, shell := range []string{"bash", "zsh"} {
		got, err := Script(shell)
		if err != nil {
			t.Fatalf("Script(%q): %v", shell, err)
		}
		for _, line := range strings.Split(got, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "PROMPT_COMMAND=") || strings.HasPrefix(trimmed, "precmd_functions=") {
				t.Fatalf("Script(%q) assigns with bare `=` instead of appending: %q", shell, line)
			}
		}
	}
}
