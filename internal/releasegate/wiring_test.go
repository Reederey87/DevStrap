package releasegate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReleaseWorkflowInvokesTheGate closes the one way this package can be
// entirely correct and entirely useless.
//
// release.yml runs only on a `v*` tag push, so no ordinary CI run ever
// exercises it. Delete the `run: go run ./cmd/release-gate` line and every
// other test in this package still passes — the truth table is unchanged, the
// binary still builds, and the gate simply never executes. This test is the
// only thing standing between "the deadline is enforced" and "the deadline is
// enforced by code nobody calls".
//
// Both call sites are required and they are not redundant: the goreleaser job
// fails the build early, and stable-publish is concurrency-grouped so it can
// run arbitrarily later than the build that produced the artifact — it is the
// last point at which a Gatekeeper-failing cask can be stopped from reaching
// the tap.
func TestReleaseWorkflowInvokesTheGate(t *testing.T) {
	path := filepath.Join("..", "..", ".github", "workflows", "release.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	workflow := string(raw)

	const invocation = "go run ./cmd/release-gate"
	got := strings.Count(workflow, invocation)
	if got != 2 {
		t.Fatalf("release.yml invokes %q %d time(s), want 2 "+
			"(once in the goreleaser job before the build, once in stable-publish before the tap push). "+
			"A gate the workflow does not call enforces nothing.", invocation, got)
	}

	// The gate reads two inputs, and BOTH must reach BOTH call sites.
	//
	// Scoped per invocation on purpose: a whole-file `strings.Contains` would
	// pass even if one of the two steps lost its `MACOS_SIGN_P12`, because the
	// name also appears in GoReleaser's own env block further down. That is the
	// failure this test has to catch — a gate that still runs but can no longer
	// see whether notarization is configured reports "prerelease, warn" and
	// waves the release through.
	steps := strings.SplitAfter(workflow, invocation)
	for i, step := range steps[:len(steps)-1] {
		// The step that owns this invocation begins at the nearest preceding
		// list item; anything earlier belongs to a different step.
		block := step
		if start := strings.LastIndex(step, "\n      - "); start >= 0 {
			block = step[start:]
		}
		for _, env := range []string{"DEVSTRAP_RELEASE_STABLE", "MACOS_SIGN_P12"} {
			if !strings.Contains(block, env) {
				t.Errorf("release-gate invocation #%d does not receive %s in its own step env; "+
					"the gate would decide on a default instead of the real value", i+1, env)
			}
		}
	}
}

// TestGateWatchesTheSecretGoreleaserActivatesOn pins the linkage between two
// files that must agree and have no other reason to.
//
// The gate refuses a release when MACOS_SIGN_P12 is empty; .goreleaser.yaml
// turns notarization ON via `isEnvSet "MACOS_SIGN_P12"`. Those are independent
// spellings of one fact. Rename the secret in one file only and the failure is
// silent and inverted: goreleaser stops notarizing while the gate still sees a
// value and passes, which is precisely the state — shipping un-notarized past
// the cutoff while every check is green — that this package exists to prevent.
func TestGateWatchesTheSecretGoreleaserActivatesOn(t *testing.T) {
	const secret = "MACOS_SIGN_P12"

	raw, err := os.ReadFile(filepath.Join("..", "..", ".goreleaser.yaml"))
	if err != nil {
		t.Fatalf("reading .goreleaser.yaml: %v", err)
	}
	if activation := `isEnvSet "` + secret + `"`; !strings.Contains(string(raw), activation) {
		t.Fatalf(".goreleaser.yaml no longer activates notarization on %s "+
			"(expected %s). The gate reads %s, so the two have drifted: "+
			"notarization and the check that enforces it now key off different secrets.",
			secret, activation, secret)
	}

	main, err := os.ReadFile(filepath.Join("..", "..", "cmd", "release-gate", "main.go"))
	if err != nil {
		t.Fatalf("reading cmd/release-gate/main.go: %v", err)
	}
	if !strings.Contains(string(main), `getenv("`+secret+`")`) {
		t.Fatalf("cmd/release-gate no longer reads %s; it must key off the same secret "+
			".goreleaser.yaml activates notarization on", secret)
	}
}
