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
