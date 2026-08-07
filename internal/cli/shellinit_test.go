package cli

import (
	"strings"
	"testing"
)

// TestShellInitKnownShells exercises `devstrap shell-init <shell>` end to end
// for every supported shell, pinning that it prints shell source (not JSON —
// shell-init deliberately does not route through opts.render/--json) that
// invokes `devstrap status --prompt`.
func TestShellInitKnownShells(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		stdout, stderr, err := executeForTest("shell-init", shell)
		if err != nil {
			t.Fatalf("shell-init %s stderr = %q err = %v", shell, stderr, err)
		}
		if !strings.Contains(stdout, "devstrap status --prompt") {
			t.Fatalf("shell-init %s output missing `devstrap status --prompt`:\n%s", shell, stdout)
		}
	}
}

// TestShellInitUnknownShell pins that an unsupported shell name is a usage
// error (exitUsage), not a crash or a silently empty snippet.
func TestShellInitUnknownShell(t *testing.T) {
	_, stderr, err := executeForTest("shell-init", "tcsh")
	if err == nil {
		t.Fatal("shell-init tcsh unexpectedly succeeded")
	}
	if !strings.Contains(stderr, "unsupported shell") {
		t.Fatalf("shell-init tcsh stderr = %q, want an \"unsupported shell\" message", stderr)
	}
}

// TestShellInitMissingArg pins that `shell-init` with no shell name is a
// usage error naming the expected argument count, matching cobra's own
// ExactArgs(1) validator wired via usageArgs.
func TestShellInitMissingArg(t *testing.T) {
	_, stderr, err := executeForTest("shell-init")
	if err == nil {
		t.Fatal("shell-init with no args unexpectedly succeeded")
	}
	if !strings.Contains(stderr, "accepts 1 arg") {
		t.Fatalf("shell-init stderr = %q, want cobra's ExactArgs usage message", stderr)
	}
}
