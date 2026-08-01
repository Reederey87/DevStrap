package main

import (
	"bytes"
	"strings"
	"testing"
)

// fakeEnv builds a getenv function over a fixed map, so the gate's input
// resolution is exercised without touching the process environment.
func fakeEnv(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

// TestRunRefusesStableReleasePastCutoff is the behavior the whole command
// exists for: on/after the Homebrew Gatekeeper cutoff, a stable tag with
// notarization dormant must stop the release.
func TestRunRefusesStableReleasePastCutoff(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(fakeEnv(map[string]string{
		"DEVSTRAP_RELEASE_STABLE": "true",
		"DEVSTRAP_RELEASE_NOW":    "2026-09-02T00:00:00Z",
	}), &stdout, &stderr)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1; a stable release past the cutoff must fail the job", code)
	}
	if !strings.Contains(stdout.String(), "::error") {
		t.Errorf("stdout carries no ::error annotation, so the refusal would not surface in the Actions summary:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "RELEASING.md") {
		t.Errorf("refusal does not point at the runbook:\n%s", stderr.String())
	}
}

func TestRunPermitsWhenNotarizationConfigured(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(fakeEnv(map[string]string{
		"DEVSTRAP_RELEASE_STABLE": "true",
		"DEVSTRAP_RELEASE_NOW":    "2026-09-02T00:00:00Z",
		"MACOS_SIGN_P12":          "not-a-real-secret",
	}), &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 when notarization is configured", code)
	}
	if strings.Contains(stdout.String(), "::error") || strings.Contains(stdout.String(), "::warning") {
		t.Errorf("a satisfied gate must be quiet, got:\n%s", stdout.String())
	}
}

func TestRunWarnsButPermitsPrereleasePastCutoff(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(fakeEnv(map[string]string{
		"DEVSTRAP_RELEASE_STABLE": "false",
		"DEVSTRAP_RELEASE_NOW":    "2026-09-02T00:00:00Z",
	}), &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; a prerelease publishes no cask and must not be blocked", code)
	}
	if !strings.Contains(stdout.String(), "::warning") {
		t.Errorf("stdout carries no ::warning; promoting this rc will be refused and nobody was told:\n%s", stdout.String())
	}
}

// TestRunFailsClosedOnUnparseableStableFlag pins the direction of the default.
// Reading an unrecognized value as `false` would silently select the LENIENT
// branch — a prerelease only ever warns — so a typo or a renamed workflow
// output would disable the refusal while the step still exited 0.
func TestRunFailsClosedOnUnparseableStableFlag(t *testing.T) {
	for _, raw := range []string{"", "TRUE", "yes", "1", "  "} {
		t.Run("value="+raw, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(fakeEnv(map[string]string{
				"DEVSTRAP_RELEASE_STABLE": raw,
				"DEVSTRAP_RELEASE_NOW":    "2026-09-02T00:00:00Z",
			}), &stdout, &stderr)

			if code != 1 {
				t.Fatalf("exit code = %d for %q, want 1; guessing here would skip the stable refusal entirely", code, raw)
			}
			if !strings.Contains(stderr.String(), "DEVSTRAP_RELEASE_STABLE") {
				t.Errorf("refusal does not name the offending variable:\n%s", stderr.String())
			}
		})
	}
}

func TestRunRejectsMalformedNowOverride(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(fakeEnv(map[string]string{
		"DEVSTRAP_RELEASE_STABLE": "true",
		"DEVSTRAP_RELEASE_NOW":    "not-a-date",
	}), &stdout, &stderr)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1; a malformed clock override must refuse rather than silently fall back to the real clock", code)
	}
}

// TestRunNeverEchoesTheSecret guards the one value in this command that must
// never be printed. Only its emptiness is read.
func TestRunNeverEchoesTheSecret(t *testing.T) {
	const secret = "S3CR3T-P12-CONTENTS"
	var stdout, stderr bytes.Buffer
	run(fakeEnv(map[string]string{
		"DEVSTRAP_RELEASE_STABLE": "true",
		"DEVSTRAP_RELEASE_NOW":    "2026-07-01T00:00:00Z",
		"MACOS_SIGN_P12":          secret,
	}), &stdout, &stderr)

	if strings.Contains(stdout.String(), secret) || strings.Contains(stderr.String(), secret) {
		t.Fatal("the notarization secret's VALUE reached the release log")
	}
}

func TestFlattenCollapsesNewlines(t *testing.T) {
	got := flatten("first line\nsecond line\n  third")
	if strings.Contains(got, "\n") {
		t.Fatalf("flatten left a newline in %q; GitHub's workflow-command syntax is line-oriented "+
			"and would truncate the annotation at the first line", got)
	}
	if got != "first line second line third" {
		t.Fatalf("flatten = %q", got)
	}
}
