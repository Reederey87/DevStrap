package hub

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/git"
	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
)

// withRecordingGCAuto swaps the package gc seam for a call recorder, restoring
// it on cleanup. Not safe with t.Parallel() (the seam is a package var); these
// tests run serially.
func withRecordingGCAuto(t *testing.T) *gcRecorder {
	t.Helper()
	rec := &gcRecorder{}
	prev := gitGCAuto
	gitGCAuto = func(ctx context.Context, runner git.Runner, dir string) {
		rec.record(dir)
		prev(ctx, runner, dir) // still exercise the real (threshold-gated) gc
	}
	t.Cleanup(func() { gitGCAuto = prev })
	return rec
}

type gcRecorder struct {
	mu   sync.Mutex
	dirs []string
}

func (r *gcRecorder) record(dir string) {
	r.mu.Lock()
	r.dirs = append(r.dirs, dir)
	r.mu.Unlock()
}

func (r *gcRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.dirs)
}

// TestGitCarrierGCAfterBatchCommit asserts the threshold-gated cache gc runs
// after a successful batch push and again after a compaction squash (P7-HUB-03),
// each time against the carrier's own clone directory.
func TestGitCarrierGCAfterBatchCommit(t *testing.T) {
	ctx := context.Background()
	rec := withRecordingGCAuto(t)
	remote := newBareCarrier(t)
	hubA := newGitCarrierTestHub(t, remote, "a")

	if err := hubA.Push(ctx, []state.Event{
		makeEvent("a1", "dev_a", 10, 1, "project.added", "{}"),
		makeEvent("a2", "dev_a", 20, 2, "project.added", "{}"),
		makeEvent("a3", "dev_a", 30, 3, "project.added", "{}"),
	}); err != nil {
		t.Fatal(err)
	}
	afterPush := rec.count()
	if afterPush < 1 {
		t.Fatalf("gc invocations after Push = %d, want >= 1", afterPush)
	}
	for _, dir := range rec.dirs {
		if dir != hubA.dir {
			t.Fatalf("gc ran against %q, want the carrier clone %q", dir, hubA.dir)
		}
	}

	if err := hubA.PutRetention(ctx, gitCarrierAdvancedManifestBytes(t, 100, map[string]int64{"dev_a": 3}), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := hubA.CompactEventsBelow(ctx, dssync.Cursor{"dev_a": 3}); err != nil {
		t.Fatal(err)
	}
	if rec.count() <= afterPush {
		t.Fatalf("gc invocations after compaction = %d, want > %d", rec.count(), afterPush)
	}
}

// readObservedFloors reads a carrier clone's observed.json off disk.
func readObservedFloors(t *testing.T, path string) map[string]time.Time {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read observed.json %s: %v", path, err)
	}
	out := map[string]time.Time{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("parse observed.json: %v", err)
	}
	return out
}

func anyKeyContains(m map[string]time.Time, sub string) bool {
	for k := range m {
		if strings.Contains(k, sub) {
			return true
		}
	}
	return false
}

// TestGitCarrierObservedPrunedAfterCompaction proves that a device whose clone
// learned observation floors before a remote compaction drops the floors for
// event objects the compaction removed, while keeping floors for survivors
// (P7-HUB-03). Without the prune, observed.json would accumulate dead keys
// forever, because remote-compaction deletions arrive via git reset — never
// through DeleteObject's forgetObservedLocked.
func TestGitCarrierObservedPrunedAfterCompaction(t *testing.T) {
	ctx := context.Background()
	remote := newBareCarrier(t)
	hubA := newGitCarrierTestHub(t, remote, "a")
	hubB := newGitCarrierTestHub(t, remote, "b")

	if err := hubA.Push(ctx, []state.Event{
		makeEvent("a1", "dev_a", 10, 1, "project.added", "{}"),
		makeEvent("a2", "dev_a", 20, 2, "project.added", "{}"),
		makeEvent("a3", "dev_a", 30, 3, "project.added", "{}"),
		makeEvent("b1", "dev_b", 15, 1, "project.added", "{}"),
	}); err != nil {
		t.Fatal(err)
	}
	// B learns floors for every event object it lists.
	if _, err := hubB.Pull(ctx, nil); err != nil {
		t.Fatal(err)
	}
	before := readObservedFloors(t, hubB.store.obsPath)
	if !anyKeyContains(before, "_a1.json") || !anyKeyContains(before, "_a2.json") {
		t.Fatalf("pre-compaction observed floors missing cold event keys: %v", keysOf(before))
	}

	// A compacts below dev_a:3, deleting the seq1/seq2 event objects and
	// squashing history (advanced manifest published first, as production does).
	if err := hubA.PutRetention(ctx, gitCarrierAdvancedManifestBytes(t, 100, map[string]int64{"dev_a": 3}), ""); err != nil {
		t.Fatal(err)
	}
	if deleted, err := hubA.CompactEventsBelow(ctx, dssync.Cursor{"dev_a": 3}); err != nil {
		t.Fatal(err)
	} else if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}

	// B pulls the squashed head from its converged cursor (above the retention
	// floor, so no snapshot is required); refreshLocked prunes the dead floors.
	if _, err := hubB.Pull(ctx, dssync.Cursor{"dev_a": 3, "dev_b": 1}); err != nil {
		t.Fatal(err)
	}
	after := readObservedFloors(t, hubB.store.obsPath)
	if anyKeyContains(after, "_a1.json") || anyKeyContains(after, "_a2.json") {
		t.Fatalf("compacted-away event floors survived the prune: %v", keysOf(after))
	}
	if !anyKeyContains(after, "_a3.json") || !anyKeyContains(after, "_b1.json") {
		t.Fatalf("surviving event floors were wrongly pruned: %v", keysOf(after))
	}
	// A successful prune advances prunedSHA to the pruned head, so the next
	// idle refresh at that head skips the walk; a failed prune would leave it
	// stale for retry (the cache-growth-fix retry invariant).
	if hubB.prunedSHA == "" || hubB.prunedSHA != hubB.fetchedSHA {
		t.Fatalf("prunedSHA = %q, fetchedSHA = %q; want a non-empty match after a successful prune", hubB.prunedSHA, hubB.fetchedSHA)
	}
}

// TestFsObjectStorePruneObservedToKeepsLive unit-tests the prune predicate:
// only keys absent from the live set are dropped (P7-HUB-03).
func TestFsObjectStorePruneObservedToKeepsLive(t *testing.T) {
	dir := t.TempDir()
	s := &fsObjectStore{root: dir, obsPath: dir + "/observed.json"}
	now := time.Now().UTC()
	s.wmu.Lock()
	s.obs = map[string]time.Time{"live": now, "dead": now}
	s.saveObsLocked()
	s.wmu.Unlock()

	s.pruneObservedTo(map[string]bool{"live": true})

	if _, ok := s.observedAt("dead"); ok {
		t.Fatal("dead key survived prune")
	}
	if _, ok := s.observedAt("live"); !ok {
		t.Fatal("live key was wrongly pruned")
	}
}

func keysOf(m map[string]time.Time) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
