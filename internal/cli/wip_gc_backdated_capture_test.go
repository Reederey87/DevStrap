package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/state"
)

// TestWipGCReapsAMinutesOldRefCapturedUnderASlowClock is the executable
// reproduction of P9-WIP-03, PINNING the open finding's behavior rather than
// asserting the desired one. If that finding is ever closed, this test must
// flip — its assertions describe the fault, not the goal.
//
// The fault: the commit-age corroboration veto (commitAgeExceeds +
// corroborateCommitAge) is one-directional. It can refuse to delete an object
// NEWER than its mirror record — the forged-mirror-age attack the 2026-07-31
// live dogfood confirmed it stops — but a commit whose OWN committer date is
// backdated is invisible to it by construction, because the date it reads is
// the one the capturing clock wrote. A device whose clock at capture is more
// than one TTL slow (the realistic instantiation is a VM or laptop restored
// from a >30-day-old snapshot, pushing WIP before NTP corrects it) stamps
// BOTH signals the sweep consults from that same wrong clock:
//
//   - the stash-create commit's committer date (git reads the system clock),
//   - the repo.wip.pushed event's HLC, and therefore the device_wip mirror
//     row's observed_at_hlc (advanceHLC takes max(wall, last_hlc), and a
//     restored snapshot's persisted last_hlc is exactly as stale as its wall
//     clock).
//
// After the clock corrects, the device's own sweep — including the AUTOMATIC
// post-sync sweep in maybeGCWipRefsAfterSync, so no explicit `wip gc` is
// needed, and clock correction via NTP is correlated with precisely the
// reconnection that triggers a sync — nominates the row as "own ref past
// TTL", fetches the object, finds a committer date past the TTL, and deletes
// a recovery ref that is minutes old in reality.
//
// This test simulates the slow capture clock with GIT_COMMITTER_DATE (the
// same value the real slow system clock would have produced) and a mirror row
// whose observed_at_hlc carries the same backdated physical time, then runs
// the real sweep with the real default TTL against a real remote.
func TestWipGCReapsAMinutesOldRefCapturedUnderASlowClock(t *testing.T) {
	opts, store, repo, remote := setupMaterializedWipGCProject(t)
	defer closeStore(store)
	ctx := context.Background()

	project, err := store.ProjectByPath(ctx, "work/proj")
	if err != nil {
		t.Fatal(err)
	}
	device, err := store.CurrentDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Capture under the slow clock: the tree is dirtied and stash-created NOW,
	// but the capturing device's clock reads TTL+1 days in the past, so the
	// commit object's sha-bound committer date is backdated by that much.
	if err := os.WriteFile(filepath.Join(repo, "wip.txt"), []byte("in-flight work"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "add", "wip.txt")
	slowClock := time.Now().Add(-defaultWipGCTTL - 24*time.Hour)
	sha := strings.TrimSpace(runGitTestEnv(t, repo,
		[]string{"GIT_COMMITTER_DATE=" + slowClock.Format(time.RFC3339)},
		"stash", "create"))
	if sha == "" {
		t.Fatal("stash create captured nothing")
	}
	ref := wipRefFor(device.ID, project.PathKey)
	runGitTest(t, repo, "push", "origin", "+"+sha+":"+ref)

	// The mirror row the push would have written: its observed_at_hlc carries
	// the same slow physical time, because nextLocalEventStamp derives the HLC
	// from max(wall clock, persisted last_hlc) and both are equally stale on a
	// restored device.
	if err := store.WithTx(ctx, func(tx *state.Tx) error {
		return tx.UpsertDeviceWipTx(ctx, device.ID, project.PathKey, project.Path, state.WipParams{
			Ref:        ref,
			SHA:        sha,
			CapturedAt: slowClock.UTC().Format(time.RFC3339),
		}, state.Event{ID: "evt_p9wip03_repro", HLC: state.HLCFromPhysicalTime(slowClock)})
	}); err != nil {
		t.Fatal(err)
	}

	// The clock has corrected (the sweep runs under the true time.Now()). The
	// ref pushed seconds ago is reaped under the DEFAULT TTL: the mirror
	// nominates it and the object-age veto, reading the same wrong clock's
	// date out of the commit, corroborates instead of refusing.
	result, failedDeletes, err := sweepWipRefs(ctx, store, opts, wipGCOptions{TTL: defaultWipGCTTL})
	if err != nil {
		t.Fatal(err)
	}
	if failedDeletes != 0 {
		t.Fatalf("failed deletes = %d, warnings %+v", failedDeletes, result.Warnings)
	}
	if len(result.Actions) != 1 {
		t.Fatalf("actions = %+v, want exactly one", result.Actions)
	}
	action := result.Actions[0]
	if !action.Delete || action.Reason != "own ref past TTL" {
		t.Fatalf("action = %+v; P9-WIP-03 documents Delete=true %q — if this now refuses, the finding was closed and this pin must be rewritten to assert the fix", action, "own ref past TTL")
	}
	out := runGitTest(t, remote, "for-each-ref", "refs/devstrap/")
	if strings.TrimSpace(out) != "" {
		t.Fatalf("remote still holds %q; the documented fault deletes the minutes-old ref", out)
	}
}
