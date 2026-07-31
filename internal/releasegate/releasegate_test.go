package releasegate

import (
	"strings"
	"testing"
	"time"
)

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// TestDecideTruthTable pins every row of the gate's decision. Each row is
// mutation-relevant: flipping the cutoff comparison, dropping the Stable check,
// or short-circuiting on NotarizeActive changes at least one expectation here.
func TestDecideTruthTable(t *testing.T) {
	cutoff := day(2026, 9, 1)

	cases := []struct {
		name           string
		stable         bool
		notarizeActive bool
		now            time.Time
		want           Verdict
	}{
		{"stable, past cutoff, dormant -> the release the gate exists to stop", true, false, day(2026, 9, 2), Fail},
		{"stable, exactly on cutoff, dormant -> cutoff day is already too late", true, false, day(2026, 9, 1), Fail},
		{"stable, one day before cutoff, dormant -> still permitted", true, false, day(2026, 8, 31), Warn},
		{"pre-release, past cutoff, dormant -> allowed, publishes no cask", false, false, day(2026, 9, 2), Warn},
		{"pre-release, before cutoff, dormant", false, false, day(2026, 8, 31), Warn},
		{"stable, past cutoff, notarization active", true, true, day(2026, 9, 2), Pass},
		{"stable, before cutoff, notarization active", true, true, day(2026, 7, 31), Pass},
		{"pre-release, past cutoff, notarization active", false, true, day(2026, 9, 2), Pass},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := Decide(Input{
				Stable:         tc.stable,
				NotarizeActive: tc.notarizeActive,
				Now:            tc.now,
				Cutoff:         cutoff,
			})
			if got != tc.want {
				t.Fatalf("Decide = %v, want %v (msg: %s)", got, tc.want, msg)
			}
			if tc.want == Pass && msg != "" {
				t.Fatalf("Pass carried a diagnostic %q; a passing gate must be silent", msg)
			}
			if tc.want != Pass && strings.TrimSpace(msg) == "" {
				t.Fatalf("%v carried no diagnostic; a gate that refuses or warns without saying why is unactionable", tc.want)
			}
		})
	}
}

// TestFailMessageIsActionable pins the content the maintainer needs at the
// moment the release stops. A refusal that says only "notarization missing"
// sends them back to the source to work out what to do.
func TestFailMessageIsActionable(t *testing.T) {
	_, msg := Decide(Input{
		Stable: true,
		Now:    day(2026, 9, 2),
		Cutoff: day(2026, 9, 1),
	})

	for _, want := range []string{
		"2026-09-01",
		"MACOS_SIGN_P12",
		"MACOS_SIGN_PASSWORD",
		"MACOS_NOTARY_KEY",
		"MACOS_NOTARY_KEY_ID",
		"MACOS_NOTARY_ISSUER_ID",
		"RELEASING.md",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message omits %q; message was:\n%s", want, msg)
		}
	}
}

// TestWarnBeforeCutoffReportsRunway pins that the pre-cutoff warning counts
// down. A warning with no number reads the same on day 200 and day 2, so it
// stops being read.
func TestWarnBeforeCutoffReportsRunway(t *testing.T) {
	verdict, msg := Decide(Input{
		Stable: true,
		Now:    day(2026, 7, 31),
		Cutoff: day(2026, 9, 1),
	})
	if verdict != Warn {
		t.Fatalf("verdict = %v, want Warn", verdict)
	}
	// 2026-07-31 -> 2026-09-01 is 32 days.
	if !strings.Contains(msg, "32 day") {
		t.Errorf("warning does not report the remaining runway; message was:\n%s", msg)
	}
}

// TestGatekeeperCutoffMatchesRunbook guards the one piece of duplicated
// knowledge: RELEASING.md and this constant describe the same deadline. If the
// date ever moves, this is where both are reconciled.
func TestGatekeeperCutoffMatchesRunbook(t *testing.T) {
	if got := GatekeeperCutoff.UTC().Format("2006-01-02"); got != "2026-09-01" {
		t.Fatalf("GatekeeperCutoff = %s, want 2026-09-01 (RELEASING.md § \"Enabling notarization\")", got)
	}
}
