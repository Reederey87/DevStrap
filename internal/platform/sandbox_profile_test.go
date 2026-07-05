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
	// The adapter now supplies pre-resolved deny lists; the pure builder renders
	// exactly those inputs. Includes a resolved leaf-symlink target
	// (/mnt/real-ssh) to prove the builder renders arbitrary caller-supplied
	// anchors, not a set derived from spec.UserHome. .kube/.docker are
	// deliberately omitted to assert the builder does NOT re-derive them.
	denyDirs := []string{
		"/Users/dev/.ssh",
		"/mnt/real-ssh",
		"/Users/dev/.aws",
		"/Users/dev/.gnupg",
		"/Users/dev/.config/gh",
		"/Users/dev/.devstrap/keys",
	}
	denyFiles := []string{
		"/Users/dev/.netrc",
		"/Users/dev/.npmrc",
		"/Users/dev/.pypirc",
		"/Users/dev/.gitconfig",
	}
	profile := sbplProfile(spec, denyDirs, denyFiles)

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
		// A resolved leaf-symlink target renders exactly as passed.
		`(subpath "/mnt/real-ssh")`,
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
	// The builder renders ONLY the caller-supplied anchors — an anchor not in
	// the deny lists must not appear even though it is a sensitiveHomeDir.
	if strings.Contains(profile, `/Users/dev/.kube`) {
		t.Fatalf("profile re-derived a home anchor not in the deny list:\n%s", profile)
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
	// Deny lists are supplied but DenySensitiveReads is false: they must be
	// ignored entirely (no deny-read block).
	profile := sbplProfile(SandboxSpec{
		WorktreeDir: "/wt",
		TmpDir:      "/tmp",
		LogDir:      "/logs",
	}, []string{"/Users/dev/.ssh"}, []string{"/Users/dev/.netrc"})
	if strings.Contains(profile, "network") {
		t.Fatalf("network deny leaked into a network-allowed profile:\n%s", profile)
	}
	if strings.Contains(profile, "file-read") {
		t.Fatalf("read denies leaked without DenySensitiveReads:\n%s", profile)
	}
	if strings.Contains(profile, ".ssh") || strings.Contains(profile, ".netrc") {
		t.Fatalf("deny lists leaked without DenySensitiveReads:\n%s", profile)
	}
}

func TestSBPLProfileEmbedsViolationTag(t *testing.T) {
	spec := SandboxSpec{
		WorktreeDir:        "/wt",
		TmpDir:             "/tmp/run",
		DenyNetwork:        true,
		DenySensitiveReads: true,
		ViolationTag:       `devstrap-sb-arun_"quoted"`,
	}
	profile := sbplProfile(spec, []string{"/Users/dev/.ssh"}, []string{"/Users/dev/.netrc"})
	wantMessage := `(with message "devstrap-sb-arun_\"quoted\"")`
	if count := strings.Count(profile, wantMessage); count != 3 {
		t.Fatalf("message count = %d, want 3 in each deny form:\n%s", count, profile)
	}
	for _, want := range []string{
		"(deny file-write*\n  " + wantMessage + "\n)",
		"(deny file-read*",
		"(deny network*\n  " + wantMessage + "\n)",
	} {
		if !strings.Contains(profile, want) {
			t.Fatalf("profile missing %q:\n%s", want, profile)
		}
	}

	untagged := sbplProfile(SandboxSpec{WorktreeDir: "/wt", TmpDir: "/tmp", DenyNetwork: true}, nil, nil)
	if strings.Contains(untagged, "with message") {
		t.Fatalf("untagged profile contains message clause:\n%s", untagged)
	}
	if !strings.Contains(untagged, "(deny file-write*)\n") || !strings.Contains(untagged, "(deny network*)\n") {
		t.Fatalf("untagged profile changed away from single-line deny forms:\n%s", untagged)
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
	if sc, err := sb.Command(t.Context(), SandboxSpec{}, []string{"true"}); err == nil {
		t.Fatal("Command() succeeded, want unsupported error")
	} else {
		sc.Cleanup() // must always be safe to call
	}
}
