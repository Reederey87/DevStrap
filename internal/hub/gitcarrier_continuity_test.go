package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
)

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func remoteHead(t *testing.T, remote string) string {
	t.Helper()
	return gitOutput(t, "", "--git-dir", remote, "rev-parse", "refs/heads/main")
}

func forceParentlessCurrentTree(t *testing.T, remote string) string {
	t.Helper()
	scratch := filepath.Join(t.TempDir(), "parentless")
	runGit(t, "", "clone", "--quiet", remote, scratch)
	tree := gitOutput(t, scratch, "rev-parse", "HEAD^{tree}")
	sha := gitOutput(t, scratch,
		"-c", "user.name=test", "-c", "user.email=test@localhost",
		"commit-tree", tree, "-m", "host rewrite")
	runGit(t, scratch, "push", "--quiet", "--force", "origin", sha+":refs/heads/main")
	return sha
}

func assertContinuityRefusal(t *testing.T, err error, contains string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), contains) {
		t.Fatalf("operation error = %v, want %q", err, contains)
	}
}

func TestGitCarrierBranchDeleteRefusesToRefound(t *testing.T) {
	ctx := context.Background()
	remote := newBareCarrier(t)
	h := newGitCarrierTestHub(t, remote, "device")
	if err := h.Push(ctx, []state.Event{makeEvent("kept", "dev_a", 10, 1, "project.added", "{}")}); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(t.TempDir(), "delete")
	runGit(t, "", "clone", "--quiet", remote, scratch)
	runGit(t, "", "--git-dir", remote, "config", "receive.denyDeleteCurrent", "ignore")
	runGit(t, scratch, "push", "--quiet", "origin", ":refs/heads/main")

	_, err := h.Pull(ctx, nil)
	assertContinuityRefusal(t, err, "no longer exists")
	assertContinuityRefusal(t, err, "refusing to re-found")
	err = h.Push(ctx, []state.Event{makeEvent("must_not_land", "dev_a", 20, 2, "project.added", "{}")})
	assertContinuityRefusal(t, err, "refusing to re-found")
	if got := strings.TrimSpace(gitLsRemote(t, remote)); got != "" {
		t.Fatalf("refused operation re-pushed deleted branch: %q", got)
	}
}

func TestGitCarrierForcePushRewindRefusedOnObservedDevicesAndCacheRemovalRecovers(t *testing.T) {
	ctx := context.Background()
	remote := newBareCarrier(t)
	hubA := newGitCarrierTestHub(t, remote, "a")
	if err := hubA.Push(ctx, []state.Event{makeEvent("old", "dev_a", 10, 1, "project.added", "{}")}); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(t.TempDir(), "rewind")
	runGit(t, "", "clone", "--quiet", remote, scratch)
	if err := hubA.Push(ctx, []state.Event{makeEvent("new", "dev_a", 20, 2, "project.added", "{}")}); err != nil {
		t.Fatal(err)
	}
	hubB := newGitCarrierTestHub(t, remote, "b")
	if _, err := hubB.Pull(ctx, nil); err != nil {
		t.Fatal(err)
	}
	runGit(t, scratch, "push", "--quiet", "--force", "origin", "HEAD:refs/heads/main")

	_, err := hubA.Pull(ctx, nil)
	assertContinuityRefusal(t, err, "carrier history was rewritten")
	_, err = hubB.Pull(ctx, nil)
	assertContinuityRefusal(t, err, "carrier history was rewritten")

	base := filepath.Dir(hubB.dir)
	if err := os.RemoveAll(base); err != nil {
		t.Fatal(err)
	}
	events, err := hubB.Pull(ctx, nil)
	if err != nil {
		t.Fatalf("Pull after explicit cache recovery: %v", err)
	}
	if !sameEventIDs(events, []string{"old"}) {
		t.Fatalf("recovered Pull ids = %v, want rewound remote state", ids(events))
	}
}

func TestGitCarrierCompactingDevicePersistsSquashedHeadAndRetention(t *testing.T) {
	ctx := context.Background()
	remote := newBareCarrier(t)
	h := newGitCarrierTestHub(t, remote, "compactor")
	if err := h.Push(ctx, []state.Event{
		makeEvent("a1", "dev_a", 10, 1, "project.added", "{}"),
		makeEvent("a2", "dev_a", 20, 2, "project.added", "{}"),
	}); err != nil {
		t.Fatal(err)
	}
	manifest := gitCarrierAdvancedManifestBytes(t, 200, map[string]int64{"dev_a": 2})
	if err := h.PutRetention(ctx, manifest, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.CompactEventsBelow(ctx, dssync.Cursor{"dev_a": 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pull(ctx, dssync.Cursor{"dev_a": 1}); err != nil {
		t.Fatalf("compacting device Pull after squash: %v", err)
	}
	state, ok, err := h.loadHeadState()
	if err != nil || !ok {
		t.Fatalf("load head state = %+v, %v, %v", state, ok, err)
	}
	sum := sha256.Sum256(manifest)
	if state.SHA != remoteHead(t, remote) || state.RetentionSHA256 != hex.EncodeToString(sum[:]) || state.RetentionProducedAt != 200 || state.RetentionFloors["dev_a"] != 2 {
		t.Fatalf("persisted compacted head = %+v", state)
	}
}

func TestGitCarrierSecondDeviceAcceptsRetentionAdvancingCompaction(t *testing.T) {
	ctx := context.Background()
	remote := newBareCarrier(t)
	hubA := newGitCarrierTestHub(t, remote, "a")
	if err := hubA.Push(ctx, []state.Event{
		makeEvent("a1", "dev_a", 10, 1, "project.added", "{}"),
		makeEvent("a2", "dev_a", 20, 2, "project.added", "{}"),
	}); err != nil {
		t.Fatal(err)
	}
	oldManifest := gitCarrierAdvancedManifestBytes(t, 100, map[string]int64{"dev_a": 1})
	if err := hubA.PutRetention(ctx, oldManifest, ""); err != nil {
		t.Fatal(err)
	}
	hubB := newGitCarrierTestHub(t, remote, "b")
	if _, err := hubB.Pull(ctx, dssync.Cursor{"dev_a": 1}); err != nil {
		t.Fatal(err)
	}
	_, etag, err := hubA.GetRetention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	newManifest := gitCarrierAdvancedManifestBytes(t, 200, map[string]int64{"dev_a": 2})
	if err := hubA.PutRetention(ctx, newManifest, etag); err != nil {
		t.Fatal(err)
	}
	if _, err := hubA.CompactEventsBelow(ctx, dssync.Cursor{"dev_a": 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := hubB.Pull(ctx, dssync.Cursor{"dev_a": 1}); err != nil {
		t.Fatalf("second device rejected legitimate compaction: %v", err)
	}
	state, ok, err := hubB.loadHeadState()
	if err != nil || !ok || state.SHA != remoteHead(t, remote) || state.RetentionProducedAt != 200 {
		t.Fatalf("second-device head state = %+v, %v, %v", state, ok, err)
	}
}

// TestGitCarrierParentlessRewriteDroppingEventsIsRefused: a parentless rewrite
// that keeps the current manifest bytes is fingerprint-identical to a real
// squash — the content gate refuses it because it deletes an event object AT
// OR ABOVE the floors, which no compaction produces. (A same-tree flatten that
// drops nothing is deliberately accepted: it loses no data and is exactly what
// a crash-recovering compactor's squash looks like.)
func TestGitCarrierParentlessRewriteDroppingEventsIsRefused(t *testing.T) {
	ctx := context.Background()
	remote := newBareCarrier(t)
	h := newGitCarrierTestHub(t, remote, "device")
	if err := h.Push(ctx, []state.Event{
		makeEvent("a1", "dev_a", 10, 1, "project.added", "{}"),
		makeEvent("a2", "dev_a", 20, 2, "project.added", "{}"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.PutRetention(ctx, gitCarrierAdvancedManifestBytes(t, 100, map[string]int64{"dev_a": 1}), ""); err != nil {
		t.Fatal(err)
	}
	// Drop the seq-2 event (>= floor 1) in a parentless rewrite that keeps the
	// manifest untouched.
	scratch := filepath.Join(t.TempDir(), "drop-event")
	runGit(t, "", "clone", "--quiet", remote, scratch)
	matches, err := filepath.Glob(filepath.Join(scratch, "workspaces", "ws_test", "eventlog", "dev_a", "*_a2.json"))
	if err != nil || len(matches) == 0 {
		var tree []string
		_ = filepath.Walk(scratch, func(p string, info os.FileInfo, _ error) error {
			if info != nil && !info.IsDir() {
				tree = append(tree, strings.TrimPrefix(p, scratch))
			}
			return nil
		})
		t.Fatalf("locate seq-2 event object: %v %v\ntree:\n%s", matches, err, strings.Join(tree, "\n"))
	}
	for _, m := range matches {
		if err := os.Remove(m); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, scratch, "add", "-A")
	runGit(t, scratch,
		"-c", "user.name=test", "-c", "user.email=test@localhost",
		"commit", "--quiet", "-m", "staged drop")
	tree := gitOutput(t, scratch, "rev-parse", "HEAD^{tree}")
	sha := gitOutput(t, scratch,
		"-c", "user.name=test", "-c", "user.email=test@localhost",
		"commit-tree", tree, "-m", "host rewrite")
	runGit(t, scratch, "push", "--quiet", "--force", "origin", sha+":refs/heads/main")

	_, err = h.Pull(ctx, dssync.Cursor{"dev_a": 1})
	assertContinuityRefusal(t, err, "carrier history was rewritten")
	assertContinuityRefusal(t, err, "deletes event object")
}

func TestGitCarrierFreshEmptyCarrierFoundsAndCreatesHeadState(t *testing.T) {
	ctx := context.Background()
	remote := newBareCarrier(t)
	h := newGitCarrierTestHub(t, remote, "founder")
	if _, _, err := h.loadHeadState(); err != nil {
		t.Fatal(err)
	}
	if err := h.Push(ctx, []state.Event{makeEvent("first", "dev_a", 10, 1, "project.added", "{}")}); err != nil {
		t.Fatal(err)
	}
	state, ok, err := h.loadHeadState()
	if err != nil || !ok || state.SHA != remoteHead(t, remote) {
		t.Fatalf("founded head state = %+v, %v, %v", state, ok, err)
	}
}

func TestGitCarrierCorruptHeadStateFailsClosedWithRecoveryPath(t *testing.T) {
	ctx := context.Background()
	h := newGitCarrierTestHub(t, newBareCarrier(t), "device")
	if err := h.Push(ctx, []state.Event{makeEvent("first", "dev_a", 10, 1, "project.added", "{}")}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.headPath, []byte("{garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := h.Pull(ctx, nil)
	assertContinuityRefusal(t, err, "head.json")
	assertContinuityRefusal(t, err, "rm -rf")
}

func TestGitCarrierSuccessfulPushPersistsRemoteHead(t *testing.T) {
	ctx := context.Background()
	remote := newBareCarrier(t)
	h := newGitCarrierTestHub(t, remote, "device")
	if err := h.Push(ctx, []state.Event{makeEvent("first", "dev_a", 10, 1, "project.added", "{}")}); err != nil {
		t.Fatal(err)
	}
	if err := h.Push(ctx, []state.Event{makeEvent("second", "dev_a", 20, 2, "project.added", "{}")}); err != nil {
		t.Fatal(err)
	}
	state, ok, err := h.loadHeadState()
	if err != nil || !ok {
		t.Fatalf("load head state = %+v, %v, %v", state, ok, err)
	}
	if want := remoteHead(t, remote); state.SHA != want {
		t.Fatalf("head.json sha = %s, remote head = %s", state.SHA, want)
	}
}

// forceParentlessWithRetention rewrites the carrier branch to a single
// parentless commit whose tree is the current tree with the retention manifest
// replaced by the given bytes — a rewrite that tries to LOOK like compaction.
func forceParentlessWithRetention(t *testing.T, remote string, manifest []byte) {
	t.Helper()
	scratch := filepath.Join(t.TempDir(), "parentless-retention")
	runGit(t, "", "clone", "--quiet", remote, scratch)
	retPath := filepath.Join(scratch, "workspaces", "ws_test", "meta", "retention.json")
	if err := os.MkdirAll(filepath.Dir(retPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(retPath, manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, scratch, "add", "-A")
	runGit(t, scratch,
		"-c", "user.name=test", "-c", "user.email=test@localhost",
		"commit", "--quiet", "-m", "staged rewrite")
	tree := gitOutput(t, scratch, "rev-parse", "HEAD^{tree}")
	sha := gitOutput(t, scratch,
		"-c", "user.name=test", "-c", "user.email=test@localhost",
		"commit-tree", tree, "-m", "host rewrite")
	runGit(t, scratch, "push", "--quiet", "--force", "origin", sha+":refs/heads/main")
}

// TestGitCarrierObserverOfAdvancedTipAcceptsSquash pins the production compact
// ordering (PutRetention on a normal commit, THEN the parentless squash reusing
// the same manifest bytes): a device that pulled between the two must accept
// the squash via the identical-fingerprint rule, not be told the carrier was
// rewritten (P7-HUB-02 review Major).
func TestGitCarrierObserverOfAdvancedTipAcceptsSquash(t *testing.T) {
	ctx := context.Background()
	remote := newBareCarrier(t)
	hubA := newGitCarrierTestHub(t, remote, "a")
	if err := hubA.Push(ctx, []state.Event{
		makeEvent("a1", "dev_a", 10, 1, "project.added", "{}"),
		makeEvent("a2", "dev_a", 20, 2, "project.added", "{}"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := hubA.PutRetention(ctx, gitCarrierAdvancedManifestBytes(t, 200, map[string]int64{"dev_a": 2}), ""); err != nil {
		t.Fatal(err)
	}
	// B observes the advanced PRE-squash tip — the window the strict-advance
	// rule got wrong.
	hubB := newGitCarrierTestHub(t, remote, "b")
	if _, err := hubB.Pull(ctx, dssync.Cursor{"dev_a": 1}); err != nil {
		t.Fatal(err)
	}
	stateB, ok, err := hubB.loadHeadState()
	if err != nil || !ok || stateB.RetentionProducedAt != 200 {
		t.Fatalf("pre-squash head state = %+v, %v, %v", stateB, ok, err)
	}
	if _, err := hubA.CompactEventsBelow(ctx, dssync.Cursor{"dev_a": 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := hubB.Pull(ctx, dssync.Cursor{"dev_a": 1}); err != nil {
		t.Fatalf("observer of the advanced tip rejected the squash: %v", err)
	}
	stateB, ok, err = hubB.loadHeadState()
	if err != nil || !ok || stateB.SHA != remoteHead(t, remote) {
		t.Fatalf("post-squash head state = %+v, %v, %v", stateB, ok, err)
	}
}

// TestGitCarrierCompactorWithStaleHeadStateAcceptsOwnSquash simulates a crash
// after the compaction force-push but before head.json was rewritten: the
// stale head.json names the pre-squash tip whose manifest bytes equal the
// squash's, so the identical-fingerprint rule self-heals on the next refresh.
func TestGitCarrierCompactorWithStaleHeadStateAcceptsOwnSquash(t *testing.T) {
	ctx := context.Background()
	remote := newBareCarrier(t)
	h := newGitCarrierTestHub(t, remote, "compactor")
	if err := h.Push(ctx, []state.Event{
		makeEvent("a1", "dev_a", 10, 1, "project.added", "{}"),
		makeEvent("a2", "dev_a", 20, 2, "project.added", "{}"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.PutRetention(ctx, gitCarrierAdvancedManifestBytes(t, 200, map[string]int64{"dev_a": 2}), ""); err != nil {
		t.Fatal(err)
	}
	preSquash, err := os.ReadFile(h.headPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.CompactEventsBelow(ctx, dssync.Cursor{"dev_a": 2}); err != nil {
		t.Fatal(err)
	}
	// Crash simulation: the squashed-head save never happened.
	if err := os.WriteFile(h.headPath, preSquash, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pull(ctx, dssync.Cursor{"dev_a": 1}); err != nil {
		t.Fatalf("compactor with stale head state rejected its own squash: %v", err)
	}
}

// TestGitCarrierFloorRegressionOnNonDescendantRefused: a rewrite that bumps
// ProducedAt but walks a device's floor BACKWARD is not compaction.
func TestGitCarrierFloorRegressionOnNonDescendantRefused(t *testing.T) {
	ctx := context.Background()
	remote := newBareCarrier(t)
	h := newGitCarrierTestHub(t, remote, "device")
	if err := h.Push(ctx, []state.Event{
		makeEvent("a1", "dev_a", 10, 1, "project.added", "{}"),
		makeEvent("a2", "dev_a", 20, 2, "project.added", "{}"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.PutRetention(ctx, gitCarrierAdvancedManifestBytes(t, 100, map[string]int64{"dev_a": 2}), ""); err != nil {
		t.Fatal(err)
	}
	forceParentlessWithRetention(t, remote, gitCarrierAdvancedManifestBytes(t, 300, map[string]int64{"dev_a": 1}))
	_, err := h.Pull(ctx, dssync.Cursor{"dev_a": 1})
	assertContinuityRefusal(t, err, "carrier history was rewritten")
}

// TestGitCarrierNonDescendantWithoutRetentionRefused: with a verified head on
// record and NO manifest anywhere, a parentless rewrite has nothing that could
// explain it — refuse.
func TestGitCarrierNonDescendantWithoutRetentionRefused(t *testing.T) {
	ctx := context.Background()
	remote := newBareCarrier(t)
	h := newGitCarrierTestHub(t, remote, "device")
	if err := h.Push(ctx, []state.Event{makeEvent("a1", "dev_a", 10, 1, "project.added", "{}")}); err != nil {
		t.Fatal(err)
	}
	forceParentlessCurrentTree(t, remote)
	_, err := h.Pull(ctx, dssync.Cursor{"dev_a": 1})
	assertContinuityRefusal(t, err, "carrier history was rewritten")
}
