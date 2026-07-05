package platform

import (
	"strings"
	"testing"
)

func TestSBPLProfileConfinesWritesAndQuotesPaths(t *testing.T) {
	spec := SandboxSpec{
		WorktreeDir:        `/private/tmp/agent worktree`,
		TmpDir:             "/private/var/folders/xx/T",
		LogDir:             "/Users/dev/.devstrap/logs/agent-runs",
		UserHome:           "/Users/dev",
		DevstrapHome:       "/Users/dev/.devstrap",
		DenyNetwork:        true,
		DenySensitiveReads: true,
	}
	profile := sbplProfile(spec)

	for _, want := range []string{
		"(version 1)",
		"(allow default)",
		"(deny file-write*)",
		// Paths with spaces stay one quoted literal.
		`(subpath "/private/tmp/agent worktree")`,
		`(subpath "/private/var/folders/xx/T")`,
		`(literal "/dev/null")`,
		"(deny file-read*",
		`(subpath "/Users/dev/.ssh")`,
		`(subpath "/Users/dev/.aws")`,
		`(subpath "/Users/dev/.gnupg")`,
		`(subpath "/Users/dev/.config/gh")`,
		`(literal "/Users/dev/.netrc")`,
		`(literal "/Users/dev/.npmrc")`,
		`(literal "/Users/dev/.pypirc")`,
		`(literal "/Users/dev/.gitconfig")`,
		`(subpath "/Users/dev/.devstrap/keys")`,
		"(deny network*)",
	} {
		if !strings.Contains(profile, want) {
			t.Fatalf("profile missing %q:\n%s", want, profile)
		}
	}
	// LogDir is profile placement only — the child must not be able to
	// rewrite its own log or profile.
	if strings.Contains(profile, `"/Users/dev/.devstrap/logs/agent-runs"`) {
		t.Fatalf("LogDir leaked into the profile allow list:\n%s", profile)
	}
	// The write denial must come BEFORE the re-allow block (SBPL applies the
	// most specific matching rule, but ordering the deny first keeps the
	// profile readable and mirrors the documented pattern).
	if strings.Index(profile, "(deny file-write*)") > strings.Index(profile, `(allow file-write*`) {
		t.Fatalf("deny file-write* must precede the allow block:\n%s", profile)
	}
}

func TestSBPLProfileOmitsOptionalDenies(t *testing.T) {
	profile := sbplProfile(SandboxSpec{
		WorktreeDir: "/wt",
		TmpDir:      "/tmp",
		LogDir:      "/logs",
	})
	if strings.Contains(profile, "network") {
		t.Fatalf("network deny leaked into a network-allowed profile:\n%s", profile)
	}
	if strings.Contains(profile, "file-read") {
		t.Fatalf("read denies leaked without DenySensitiveReads:\n%s", profile)
	}
}

func TestSBPLQuoteEscapesQuotesAndBackslashes(t *testing.T) {
	got := sbplQuote(`/tmp/we"ird\path`)
	want := `"/tmp/we\"ird\\path"`
	if got != want {
		t.Fatalf("sbplQuote = %s, want %s", got, want)
	}
}

func TestUnsupportedSandboxReportsUnsupported(t *testing.T) {
	sb := UnsupportedSandbox{Platform: "plan9"}
	if err := sb.Available(); err == nil || !strings.Contains(err.Error(), "plan9") {
		t.Fatalf("Available() = %v, want unsupported error naming the platform", err)
	}
	if _, cleanup, err := sb.Command(t.Context(), SandboxSpec{}, []string{"true"}); err == nil {
		t.Fatal("Command() succeeded, want unsupported error")
	} else {
		cleanup() // must always be safe to call
	}
}
