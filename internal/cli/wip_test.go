package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/state"
)

// TestWipRowsForProject pins wipRowsForProject's pure-function contract
// (mirroring TestGitstateRowsForProject): unlike gitstateRowsForProject,
// there is no "never synced"-style forced row — zero pending WIP rows must
// map to a nil (absent) result, not an empty-but-present placeholder, since
// having nothing pending is the normal, healthy state for most projects most
// of the time.
func TestWipRowsForProject(t *testing.T) {
	now := time.Now()

	if got := wipRowsForProject(nil, now); got != nil {
		t.Fatalf("zero-row rows = %+v, want nil (absent section), not a forced/placeholder row", got)
	}

	rows := []state.DeviceWip{{
		DeviceID: "dev_peer", Ref: "refs/devstrap/wip/dev_peer/work/proj",
		SHA: "abc123", BaseSHA: "def456",
		ObservedAtHLC: state.HLCFromPhysicalTime(now.Add(-90 * time.Minute)),
	}}
	got := wipRowsForProject(rows, now)
	if len(got) != 1 {
		t.Fatalf("populated rows = %+v, want exactly one row", got)
	}
	row := got[0]
	if row.DeviceID != "dev_peer" || row.Ref != rows[0].Ref || row.SHA != "abc123" || row.BaseSHA != "def456" {
		t.Fatalf("row = %+v, want fields copied verbatim from state.DeviceWip", row)
	}
	if !strings.Contains(row.Observed, "captured") {
		t.Fatalf("row.Observed = %q, want a captured-ago age string", row.Observed)
	}
}

// TestResolveWipTarget pins the shared device-resolution helper `wip
// show`/`wip apply`/`wip drop` all call: an unknown explicit --device is a
// usage error naming it; an empty --device with exactly one row picks it
// automatically; an empty --device with 2+ rows is an ambiguous usage error
// listing every candidate.
func TestResolveWipTarget(t *testing.T) {
	rowA := state.DeviceWip{DeviceID: "dev_a", Ref: "refs/devstrap/wip/dev_a/work/proj", SHA: "aaa"}
	rowB := state.DeviceWip{DeviceID: "dev_b", Ref: "refs/devstrap/wip/dev_b/work/proj", SHA: "bbb"}

	t.Run("unknown device", func(t *testing.T) {
		_, err := resolveWipTarget([]state.DeviceWip{rowA}, "dev_nope", "work/proj")
		if err == nil {
			t.Fatal("want error for unknown --device, got nil")
		}
		if !strings.Contains(err.Error(), "no pending WIP for device dev_nope on work/proj") {
			t.Fatalf("err = %q, want it to name the unknown device and project", err.Error())
		}
	})

	t.Run("single row no device flag", func(t *testing.T) {
		got, err := resolveWipTarget([]state.DeviceWip{rowA}, "", "work/proj")
		if err != nil {
			t.Fatalf("resolveWipTarget with a single candidate returned error: %v", err)
		}
		if got.DeviceID != "dev_a" {
			t.Fatalf("got.DeviceID = %q, want dev_a", got.DeviceID)
		}
	})

	t.Run("ambiguous multiple rows no device flag", func(t *testing.T) {
		_, err := resolveWipTarget([]state.DeviceWip{rowA, rowB}, "", "work/proj")
		if err == nil {
			t.Fatal("want ambiguous usage error for multiple candidates with no --device, got nil")
		}
		msg := err.Error()
		if !strings.Contains(msg, "multiple devices have pending WIP for work/proj") ||
			!strings.Contains(msg, "dev_a") || !strings.Contains(msg, "dev_b") ||
			!strings.Contains(msg, "--device") {
			t.Fatalf("err = %q, want it to name every candidate and point at --device", msg)
		}
	})

	t.Run("explicit device among multiple rows", func(t *testing.T) {
		got, err := resolveWipTarget([]state.DeviceWip{rowA, rowB}, "dev_b", "work/proj")
		if err != nil {
			t.Fatalf("resolveWipTarget with explicit --device returned error: %v", err)
		}
		if got.DeviceID != "dev_b" {
			t.Fatalf("got.DeviceID = %q, want dev_b", got.DeviceID)
		}
	})
}
