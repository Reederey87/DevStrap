// Package releasegate decides whether a release may publish a macOS artifact
// that Homebrew will still accept.
//
// P4-SEC-05 background. DevStrap's release binaries are ad-hoc signed, so the
// quarantine bit Homebrew sets on cask artifacts makes Gatekeeper refuse them;
// the cask works around that with an `xattr -dr com.apple.quarantine`
// post-install hook (`.goreleaser.yaml`). Homebrew removes support for
// Gatekeeper-failing casks on 2026-09-01. The `notarize:` block is already
// wired and activates the moment the five MACOS_* secrets exist — the config
// is complete and correct.
//
// What was missing is a gate. Nothing failed if that date passed with
// notarization still dormant: a stable tag cut on 2026-09-02 would publish a
// cask Homebrew refuses, and the pipeline would report success. A deadline
// that only exists in prose is one a release cannot check.
package releasegate

import (
	"fmt"
	"time"
)

// GatekeeperCutoff is the date Homebrew stops accepting casks whose binaries
// fail Gatekeeper. Sourced from RELEASING.md § "Enabling notarization"; the two
// must not drift, which is why the runbook cites this constant rather than
// restating the date.
var GatekeeperCutoff = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

// Verdict is the gate's decision.
type Verdict int

const (
	// Pass permits the release with no diagnostic.
	Pass Verdict = iota
	// Warn permits the release but reports remaining runway, or reports that
	// the next stable tag will be refused.
	Warn
	// Fail refuses the release.
	Fail
)

func (v Verdict) String() string {
	switch v {
	case Pass:
		return "pass"
	case Warn:
		return "warn"
	case Fail:
		return "fail"
	default:
		return fmt.Sprintf("Verdict(%d)", int(v))
	}
}

// Input is everything the decision depends on. Now and Cutoff are parameters
// rather than reads of the ambient clock so the truth table is testable without
// waiting for a calendar date to arrive — a gate whose only proof is "run it in
// September" is a gate nobody verifies.
type Input struct {
	// Stable is true for a vX.Y.Z tag and false for a pre-release (-rc.N).
	// Only a stable tag publishes the Homebrew cask (`skip_upload: auto`), so
	// only a stable tag can put a refused artifact in front of users.
	Stable bool
	// NotarizeActive reports whether notarization is switched on. It mirrors
	// the `{{ isEnvSet "MACOS_SIGN_P12" }}` expression that activates the
	// .goreleaser.yaml notarize block. It is a boolean by construction: the
	// secret's VALUE must never reach this package.
	NotarizeActive bool
	Now            time.Time
	Cutoff         time.Time
}

// Decide returns the verdict and a human-readable diagnostic. The diagnostic is
// empty only for Pass.
//
// The truth table, with the reason each row is what it is:
//
//	notarization active              -> Pass. Nothing to warn about.
//	dormant, past cutoff, stable     -> Fail. This is the release that ships a
//	                                   cask Homebrew refuses. Refusing here is
//	                                   the entire point of the gate.
//	dormant, past cutoff, pre-release-> Warn. An rc publishes no cask, so it is
//	                                   not itself broken — but promoting it
//	                                   will be refused, and finding that out at
//	                                   promotion time is too late to be useful.
//	dormant, before cutoff           -> Warn with remaining runway, so every
//	                                   release between now and the cutoff
//	                                   reports the countdown unprompted.
func Decide(in Input) (Verdict, string) {
	if in.NotarizeActive {
		return Pass, ""
	}

	cutoff := in.Cutoff.UTC().Format("2006-01-02")

	if !in.Now.Before(in.Cutoff) {
		if in.Stable {
			return Fail, fmt.Sprintf(
				"macOS notarization is not configured, and the Homebrew Gatekeeper cutoff (%s) has passed.\n"+
					"A stable tag published now ships a cask Homebrew refuses: the binaries are ad-hoc signed, "+
					"and the cask's quarantine-strip hook is no longer an accepted workaround.\n"+
					"To proceed, complete Apple Developer enrollment and set all five secrets in one sitting:\n"+
					"  MACOS_SIGN_P12, MACOS_SIGN_PASSWORD, MACOS_NOTARY_KEY, MACOS_NOTARY_KEY_ID, MACOS_NOTARY_ISSUER_ID\n"+
					"See RELEASING.md § \"Enabling notarization\" for the one-time enrollment checklist.",
				cutoff)
		}
		return Warn, fmt.Sprintf(
			"macOS notarization is not configured and the Homebrew Gatekeeper cutoff (%s) has passed.\n"+
				"This pre-release publishes no cask, so it is allowed — but promoting it to a stable tag "+
				"will be REFUSED by this gate until the five MACOS_* secrets are set.\n"+
				"See RELEASING.md § \"Enabling notarization\".",
			cutoff)
	}

	days := int(in.Cutoff.Sub(in.Now).Hours() / 24)
	return Warn, fmt.Sprintf(
		"macOS notarization is not configured. Homebrew drops Gatekeeper-failing casks on %s — %d day(s) of runway.\n"+
			"After that date this gate REFUSES stable releases. Enrollment is the long pole, not the config: "+
			"the .goreleaser.yaml notarize block is already wired and activates when all five MACOS_* secrets are set.\n"+
			"See RELEASING.md § \"Enabling notarization\".",
		cutoff, days)
}
