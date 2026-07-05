package hub

// Adversarial-review regression tests (PR #96): writer-clock sidecars must not
// drive destructive age decisions, a live lock holder must not be stolen from,
// and dry-run must not write.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tamperSidecar rewrites an object's timestamp sidecar on the carrier remote
// through a scratch clone, simulating a device with a skewed clock.
func tamperSidecar(t *testing.T, remote, key string, when time.Time) {
	t.Helper()
	scratch := filepath.Join(t.TempDir(), "tamper")
	runGit(t, "", "clone", "--quiet", remote, scratch)
	sidecar := filepath.Join(scratch, filepath.FromSlash(gitTimesPrefix+key))
	if err := os.MkdirAll(filepath.Dir(sidecar), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sidecar, []byte(when.UTC().Format(time.RFC3339Nano)), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, scratch, "-c", "user.name=t", "-c", "user.email=t@localhost", "add", "-A")
	runGit(t, scratch, "-c", "user.name=t", "-c", "user.email=t@localhost", "commit", "--quiet", "-m", "tamper")
	runGit(t, scratch, "push", "--quiet", "origin", "HEAD:refs/heads/main")
}

// TestGitCarrierSkewedOldSidecarCannotAgeABlob pins the observation floor: a
// writer whose clock is days slow uploads a blob whose sidecar already looks
// past any gc grace window, but a reader must report it no older than the
// reader's own first observation — so gc's "younger than the grace window"
// judgment runs on the READER's clock and the live blob survives.
func TestGitCarrierSkewedOldSidecarCannotAgeABlob(t *testing.T) {
	ctx := context.Background()
	remote := newBareCarrier(t)
	const blobSHA = "cc00000000000000000000000000000000000000000000000000000000000001"
	writer := newGitCarrierTestHub(t, remote, "writer")
	if err := writer.PutBlob(ctx, blobSHA, strings.NewReader("live ciphertext")); err != nil {
		t.Fatal(err)
	}
	tamperSidecar(t, remote, "workspaces/ws_test/blobs/"+blobSHA, time.Now().Add(-72*time.Hour))

	reader := newGitCarrierTestHub(t, remote, "reader")
	info, err := reader.StatBlob(ctx, blobSHA)
	if err != nil {
		t.Fatal(err)
	}
	if age := time.Since(info.LastModified); age > time.Minute {
		t.Fatalf("skewed sidecar aged the blob %s past the reader's observation floor", age)
	}
	list, err := reader.ListBlobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || time.Since(list[0].LastModified) > time.Minute {
		t.Fatalf("ListBlobs leaked the skewed age: %+v", list)
	}
}

// TestGitCarrierFutureSweepLockIsBreakableAfterObservedTTL pins the clamp in
// GetSweepLock: a dead holder whose clock was far in the future must not leave
// an unbreakable lock — the reported time is clamped down to this reader's
// first observation, so a TTL judged against it eventually expires.
func TestGitCarrierFutureSweepLockIsBreakableAfterObservedTTL(t *testing.T) {
	ctx := context.Background()
	remote := newBareCarrier(t)
	holder := newGitCarrierTestHub(t, remote, "holder")
	if err := holder.PutSweepLock(ctx, []byte(`{"holder_device":"dev_future"}`)); err != nil {
		t.Fatal(err)
	}
	tamperSidecar(t, remote, "workspaces/ws_test/meta/sweep.lock", time.Now().Add(365*24*time.Hour))

	reader := newGitCarrierTestHub(t, remote, "reader")
	_, mod, err := reader.GetSweepLock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if mod.After(time.Now().Add(time.Minute)) {
		t.Fatalf("future-dated sweep lock reported %s — TTL break is wedged", mod)
	}
}

// TestGitCarrierLiveLockIsNotStolenAndDeadLockIs pins the heartbeat: a waiter
// must not break a lock whose holder heartbeats (even past the stale window),
// while a lock whose holder is dead (stale mtime, no heartbeat) is broken.
func TestGitCarrierLiveLockIsNotStolenAndDeadLockIs(t *testing.T) {
	remote := newBareCarrier(t)
	holder := newGitCarrierTestHub(t, remote, "shared")
	holder.lockHeartbeat = 20 * time.Millisecond

	release, err := holder.lock()
	if err != nil {
		t.Fatal(err)
	}
	// Age the lock file past the stale window; the heartbeat must refresh it
	// so a waiter's stale-break never fires on a LIVE holder.
	old := time.Now().Add(-2 * fsLockStale)
	if err := os.Chtimes(holder.lockPath, old, old); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // several heartbeats
	info, err := os.Stat(holder.lockPath)
	if err != nil {
		t.Fatalf("live lock vanished: %v", err)
	}
	if time.Since(info.ModTime()) > fsLockStale {
		t.Fatal("heartbeat did not refresh the held lock; a waiter would steal the checkout")
	}
	release()

	// Dead holder: a lock file with a stale mtime and no heartbeat is broken.
	if err := os.WriteFile(holder.lockPath, []byte("999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(holder.lockPath, old, old); err != nil {
		t.Fatal(err)
	}
	release2, err := holder.lock()
	if err != nil {
		t.Fatalf("stale dead lock was not broken: %v", err)
	}
	release2()
}

// TestGitCarrierDryRunMigrateWritesNothing pins the dry-run contract: against
// a fresh (empty) carrier, `MigrateLegacyEvents(dryRun=true)` must not seed
// the marker, create the branch, or push anything.
func TestGitCarrierDryRunMigrateWritesNothing(t *testing.T) {
	ctx := context.Background()
	remote := newBareCarrier(t)
	h := newGitCarrierTestHub(t, remote, "dryrun")
	if _, _, err := h.MigrateLegacyEvents(ctx, true); err != nil {
		t.Fatalf("dry-run migrate: %v", err)
	}
	out := gitLsRemote(t, remote)
	if strings.TrimSpace(out) != "" {
		t.Fatalf("dry-run migrate wrote to the carrier: %q", out)
	}
	// The non-dry run against the same empty carrier is also a no-op commit
	// (no legacy layout, no mutations) and must not fail.
	if _, _, err := h.MigrateLegacyEvents(ctx, false); err != nil {
		t.Fatalf("real migrate: %v", err)
	}
}

// TestGitCarrierRefusesSymlinkedCarrierPaths pins the safePath confinement: a
// hostile carrier tree that commits `workspaces` as a symlink pointing outside
// the checkout must be refused at the object layer — reads must not follow it
// (exfiltration) and writes must not land through it (clobbering).
func TestGitCarrierRefusesSymlinkedCarrierPaths(t *testing.T) {
	ctx := context.Background()
	remote := newBareCarrier(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "victim"), []byte("host file"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Seed a hostile-but-marked carrier: valid marker + workspaces -> outside.
	scratch := filepath.Join(t.TempDir(), "scratch")
	runGit(t, "", "clone", "--quiet", remote, scratch)
	if err := os.WriteFile(filepath.Join(scratch, gitMarkerFile), []byte(`{"version":1,"workspace_id":"ws_test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(scratch, "workspaces")); err != nil {
		t.Fatal(err)
	}
	runGit(t, scratch, "-c", "user.name=t", "-c", "user.email=t@localhost", "add", "-A")
	runGit(t, scratch, "-c", "user.name=t", "-c", "user.email=t@localhost", "commit", "--quiet", "-m", "hostile")
	runGit(t, scratch, "push", "--quiet", "origin", "HEAD:refs/heads/main")

	h := newGitCarrierTestHub(t, remote, "victim-reader")
	const blobSHA = "dd00000000000000000000000000000000000000000000000000000000000001"
	if _, err := h.GetBlob(ctx, blobSHA); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("GetBlob through a symlinked component = %v, want symlink refusal", err)
	}
	if err := h.PutBlob(ctx, blobSHA, strings.NewReader("x")); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("PutBlob through a symlinked component = %v, want symlink refusal", err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "victim" {
		t.Fatalf("outside dir was touched through the symlink: %v", entries)
	}
}

func gitLsRemote(t *testing.T, remote string) string {
	t.Helper()
	out, err := exec.Command("git", "ls-remote", remote).CombinedOutput()
	if err != nil {
		t.Fatalf("git ls-remote: %v: %s", err, out)
	}
	return string(out)
}
