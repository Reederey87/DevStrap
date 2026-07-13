package platform

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestSandboxHelperArgsShape(t *testing.T) {
	specJSON := `{"worktree":"value with spaces"}`
	got := sandboxHelperArgs("/opt/devstrap", specJSON, []string{"--dash-first", "arg with space"})
	want := []string{"/opt/devstrap", SandboxHelperCommand, "--spec", specJSON, "--", "--dash-first", "arg with space"}
	if !slices.Equal(got, want) {
		t.Fatalf("sandboxHelperArgs = %v, want %v", got, want)
	}

	spec := SandboxSpec{
		WorktreeDir:        "/work/tree",
		TmpDir:             "/tmp/devstrap-run",
		LogDir:             "/home/dev/.devstrap/logs/agent-runs",
		UserHome:           "/home/dev",
		DevstrapHome:       "/home/dev/.devstrap",
		DenyNetwork:        true,
		DenySensitiveReads: true,
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := sandboxHelperArgs("/opt/devstrap", string(raw), []string{"/bin/true"})
	var roundTrip SandboxSpec
	if err := json.Unmarshal([]byte(wrapped[3]), &roundTrip); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(roundTrip, spec) {
		t.Fatalf("round-trip spec = %+v, want %+v", roundTrip, spec)
	}
}

func TestLandlockLimitationsPerABI(t *testing.T) {
	cases := []struct {
		abi        int
		wantNetSub string
	}{
		{abi: 3, wantNetSub: "network deny NOT enforced"},
		{abi: 4, wantNetSub: "TCP bind/connect only"},
		{abi: 6, wantNetSub: "TCP bind/connect only"},
	}
	for _, tc := range cases {
		t.Run(tc.wantNetSub, func(t *testing.T) {
			lims := landlockLimitations(tc.abi)
			if len(lims) != 2 {
				t.Fatalf("landlockLimitations(%d) len = %d, want 2: %v", tc.abi, len(lims), lims)
			}
			joined := strings.Join(lims, "\n")
			for _, want := range []string{tc.wantNetSub, "namespace"} {
				if !strings.Contains(joined, want) {
					t.Fatalf("landlockLimitations(%d) = %q, want substring %q", tc.abi, joined, want)
				}
			}
		})
	}
}

// TestLandlockLimitationsMirrorBwrapOnlyGuarantees pins that the documented
// degrade names exactly what the fallback dropped, so nobody can quietly widen
// the gap or quietly claim parity.
func TestLandlockLimitationsMirrorBwrapOnlyGuarantees(t *testing.T) {
	for _, abi := range []int{3, 4} {
		joined := strings.Join(landlockLimitations(abi), "\n")
		if !strings.Contains(joined, "network") && !strings.Contains(joined, "TCP") {
			t.Fatalf("ABI %d limitations = %q, want network degrade named", abi, joined)
		}
		if !strings.Contains(joined, "namespace") {
			t.Fatalf("ABI %d limitations = %q, want namespace degrade named", abi, joined)
		}
	}
}
