// Command release-gate refuses a stable release that would publish a Homebrew
// cask Gatekeeper rejects (P4-SEC-05).
//
// It runs in the release workflow twice: in the gated publisher job after the
// 0-or-5 MACOS_* secret validation and before GoReleaser, and again in
// stable-publish before the tap push — that job is concurrency-grouped and can
// run arbitrarily later than the build that produced the artifact, so it is the
// last point at which a Gatekeeper-failing cask can be stopped.
//
// The decision lives in internal/releasegate so the truth table is unit-tested
// rather than proven only by waiting for a calendar date.
//
// Inputs are environment variables:
//
//	DEVSTRAP_RELEASE_STABLE   exactly "true" for a vX.Y.Z tag or "false" for a
//	                          prerelease (release.yml's release-mode step
//	                          computes it). Anything else is refused, not guessed.
//	MACOS_SIGN_P12            the notarization secret. Only its EMPTINESS is
//	                          read — the value is never logged, compared, or
//	                          passed on.
//	DEVSTRAP_RELEASE_NOW      optional RFC3339 date override, for exercising the
//	                          gate itself. Never set by release.yml, and it must
//	                          not be: it is a test seam, not a release control.
//
// Exit status: 0 on pass or warn, 1 on refusal or bad input.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Reederey87/DevStrap/internal/releasegate"
)

func main() {
	os.Exit(run(os.Getenv, os.Stdout, os.Stderr))
}

// run resolves the gate's inputs from getenv and writes its verdict, returning
// the process exit code. Split out from main so it is testable without os.Exit
// tearing down the test binary, and so getenv can be faked — a gate whose only
// exercise is a real release is a gate nobody has run.
func run(getenv func(string) string, stdout, stderr io.Writer) int {
	now := time.Now().UTC()
	if raw := strings.TrimSpace(getenv("DEVSTRAP_RELEASE_NOW")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "release-gate: DEVSTRAP_RELEASE_NOW is not RFC3339: %v\n", err)
			return 1
		}
		now = parsed.UTC()
	}

	// Fail closed on anything that is not exactly true/false. Reading an
	// unrecognized value as `false` would silently pick the LENIENT branch — a
	// prerelease only ever warns — so a typo or a renamed workflow output would
	// disable the refusal while the step still exited 0. For a gate whose
	// entire job is to refuse, an unparseable input must stop the release.
	stableRaw := strings.TrimSpace(getenv("DEVSTRAP_RELEASE_STABLE"))
	var stable bool
	switch stableRaw {
	case "true":
		stable = true
	case "false":
		stable = false
	default:
		_, _ = fmt.Fprintf(stderr,
			"release-gate: DEVSTRAP_RELEASE_STABLE must be exactly \"true\" or \"false\", got %q.\n"+
				"Refusing rather than assuming a prerelease: guessing here would skip the stable-release refusal entirely.\n",
			stableRaw)
		return 1
	}

	verdict, msg := releasegate.Decide(releasegate.Input{
		Stable: stable,
		// Emptiness only. This mirrors goreleaser's own
		// `{{ isEnvSet "MACOS_SIGN_P12" }}` activation expression, and the
		// 0-or-5 step immediately upstream has already refused a partial set,
		// so this one variable is a sound proxy for all five.
		NotarizeActive: getenv("MACOS_SIGN_P12") != "",
		Now:            now,
		Cutoff:         releasegate.GatekeeperCutoff,
	})

	switch verdict {
	case releasegate.Fail:
		// ::error:: renders in the Actions summary, not just the raw log.
		_, _ = fmt.Fprintf(stdout, "::error title=Release refused: macOS notarization required::%s\n", flatten(msg))
		_, _ = fmt.Fprintln(stderr, msg)
		return 1
	case releasegate.Warn:
		_, _ = fmt.Fprintf(stdout, "::warning title=macOS notarization not configured::%s\n", flatten(msg))
		_, _ = fmt.Fprintln(stderr, msg)
	case releasegate.Pass:
		_, _ = fmt.Fprintln(stdout, "release-gate: macOS notarization is configured; Gatekeeper cutoff satisfied.")
	}
	return 0
}

// flatten collapses a multi-line diagnostic into one line. GitHub's workflow
// command syntax is line-oriented: an embedded newline would truncate the
// annotation at the first line and print the remainder as stray log output.
func flatten(msg string) string {
	return strings.Join(strings.Fields(msg), " ")
}
