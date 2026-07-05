package hub

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
)

// newFolderTestHub constructs a FolderHub over dir with cache under cacheRoot.
// Two hubs constructed with the SAME dir AND cacheRoot share one cross-process
// lock file (their lockPath keys on cacheRoot + the resolved dir), which is how
// the same-machine CAS test serializes racing writers.
func newFolderTestHub(t *testing.T, dir, cacheRoot string) *FolderHub {
	t.Helper()
	h, err := NewFolderHub(dir, "ws_test", cacheRoot)
	if err != nil {
		t.Fatalf("NewFolderHub(%s): %v", dir, err)
	}
	h.sleep = func(time.Duration) {}
	return h
}

// TestFolderHubConformance runs the shared hub contract (event log, blob plane,
// retention CAS, sealed snapshots, ack plane, sweep lock) against the folder
// carrier — the same helpers the git carrier is proven against.
func TestFolderHubConformance(t *testing.T) {
	ctx := context.Background()
	newDir := func(name string) string { return filepath.Join(t.TempDir(), name) }
	assertHubRoundTrip(t, ctx, newFolderTestHub(t, newDir("shared"), t.TempDir()))
	assertGitCarrierAckPlane(t, ctx, newFolderTestHub(t, newDir("shared"), t.TempDir()))
	assertGitCarrierRetentionAndSnapshot(t, ctx, newFolderTestHub(t, newDir("shared"), t.TempDir()))
	assertGitCarrierSweepLock(t, ctx, newFolderTestHub(t, newDir("shared"), t.TempDir()))
}

func TestNewFolderHubRejectsInvalid(t *testing.T) {
	cache := t.TempDir()
	t.Run("relative path", func(t *testing.T) {
		if _, err := NewFolderHub("relative/path", "ws_test", cache); err == nil {
			t.Fatal("NewFolderHub accepted a relative path")
		}
	})
	t.Run("empty workspace id", func(t *testing.T) {
		if _, err := NewFolderHub(filepath.Join(t.TempDir(), "d"), "", cache); err == nil {
			t.Fatal("NewFolderHub accepted an empty workspace id")
		}
	})
	t.Run("path is a file", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "afile")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewFolderHub(file, "ws_test", cache); err == nil {
			t.Fatal("NewFolderHub accepted an existing file as the folder root")
		}
	})
	t.Run("empty cache root", func(t *testing.T) {
		if _, err := NewFolderHub(filepath.Join(t.TempDir(), "d"), "ws_test", ""); err == nil {
			t.Fatal("NewFolderHub accepted an empty cache root")
		}
	})
}

// TestFolderHubSymlinkedRootResolves confirms a symlinked folder root (the
// common cloud-drive case) is resolved to its real path so the store and lock
// key on a stable directory.
func TestFolderHubSymlinkedRootResolves(t *testing.T) {
	ctx := context.Background()
	real := filepath.Join(t.TempDir(), "real-drive")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link-drive")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	h := newFolderTestHub(t, link, t.TempDir())
	if err := h.PutSweepLock(ctx, []byte(`{"holder_device":"dev_a"}`)); err != nil {
		t.Fatalf("PutSweepLock through a symlinked root: %v", err)
	}
	// The object landed under the REAL directory, not the symlink alias.
	if _, err := os.Stat(filepath.Join(real, "workspaces", "ws_test", "meta", "sweep.lock")); err != nil {
		t.Fatalf("object not written under the resolved root: %v", err)
	}
}

// TestFolderHubCrossProcessLockCASOneWinner mirrors
// TestGitCarrierRetentionCASOneWinner: two FolderHub instances sharing one
// folder AND one local cache (hence one lock file) race a retention CAS from
// the same base etag; the cross-process lock serializes the read-compare-write,
// so exactly one wins and the other sees ErrRetentionConflict. This is the
// same-machine guarantee (cross-DEVICE CAS through a cloud drive is best-effort
// by design — see the folder.go package comment).
func TestFolderHubCrossProcessLockCASOneWinner(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "shared")
	cache := t.TempDir()
	seed := newFolderTestHub(t, dir, cache)
	if err := seed.PutRetention(ctx, gitCarrierManifestBytes(t, map[string]int64{"dev_a": 1}), ""); err != nil {
		t.Fatal(err)
	}
	hubA := newFolderTestHub(t, dir, cache)
	hubB := newFolderTestHub(t, dir, cache)
	_, etagA, err := hubA.GetRetention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, etagB, err := hubB.GetRetention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	errs := runTwo(func() error {
		return hubA.PutRetention(ctx, gitCarrierManifestBytes(t, map[string]int64{"dev_a": 2}), etagA)
	}, func() error {
		return hubB.PutRetention(ctx, gitCarrierManifestBytes(t, map[string]int64{"dev_a": 3}), etagB)
	})
	assertExactlyOneErr(t, errs, dssync.ErrRetentionConflict)
}

// TestFolderHubSweepLockOneHolder confirms the create-only sweep lock admits a
// single holder across two same-machine instances sharing the lock file.
func TestFolderHubSweepLockOneHolder(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "shared")
	cache := t.TempDir()
	hubA := newFolderTestHub(t, dir, cache)
	hubB := newFolderTestHub(t, dir, cache)
	errs := runTwo(func() error {
		return hubA.PutSweepLock(ctx, []byte(`{"holder_device":"dev_a"}`))
	}, func() error {
		return hubB.PutSweepLock(ctx, []byte(`{"holder_device":"dev_b"}`))
	})
	assertExactlyOneErr(t, errs, dssync.ErrSweepLockHeld)
}

// TestFolderHubTwoDeviceConvergence is the minimal two-device path: two hubs
// with DIFFERENT local caches (distinct devices) share the folder and converge,
// including the empty-folder bootstrap.
func TestFolderHubTwoDeviceConvergence(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "shared")
	deviceA := newFolderTestHub(t, dir, t.TempDir())
	deviceB := newFolderTestHub(t, dir, t.TempDir())
	if err := deviceA.Push(ctx, []state.Event{
		makeEvent("evt_a1", "dev_a", 100, 1, "project.added", `{"path":"work/x"}`),
		makeEvent("evt_a2", "dev_a", 200, 2, "project.added", `{"path":"work/y"}`),
	}); err != nil {
		t.Fatalf("device A push: %v", err)
	}
	got, err := deviceB.Pull(ctx, dssync.Cursor{})
	if err != nil {
		t.Fatalf("device B pull: %v", err)
	}
	if !sameEventIDs(got, []string{"evt_a1", "evt_a2"}) {
		t.Fatalf("device B pull ids = %v, want evt_a1, evt_a2", ids(got))
	}
	if _, err := deviceA.CompactEventsBelow(ctx, dssync.Cursor{"dev_a": 2}); err != nil {
		t.Fatalf("CompactEventsBelow: %v", err)
	}
	got, err = newFolderTestHub(t, dir, t.TempDir()).Pull(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !sameEventIDs(got, []string{"evt_a2"}) {
		t.Fatalf("post-compact pull ids = %v, want evt_a2", ids(got))
	}
}

// TestFolderHubRefusesReplacedRoot pins the use-time root revalidation (review
// P2): the constructor resolves the root once, but a long-lived hub whose
// shared folder is later swapped for a symlink must refuse to follow it —
// safePath only Lstats components below the root, so without the guard-time
// check every subsequent read/write would silently land outside the registered
// folder.
func TestFolderHubRefusesReplacedRoot(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "shared")
	h := newFolderTestHub(t, dir, t.TempDir())
	if err := h.PutSweepLock(ctx, []byte(`{"holder_device":"dev_a"}`)); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	outside := t.TempDir()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, dir); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if err := h.PutSweepLock(ctx, []byte(`{"holder_device":"dev_a"}`)); err == nil {
		t.Fatal("write through a replaced root succeeded; want refusal")
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("outside dir entries = %v (err %v); nothing must escape the registered folder", entries, err)
	}
	if _, err := h.Pull(ctx, nil); err == nil {
		t.Fatal("read through a replaced root succeeded; want refusal")
	}
}

// TestFolderHubWorkspacesAreIsolated pins the two-workspaces/one-folder case
// (review P3): isolation rides the workspaces/<workspace_id>/ key prefix, so
// two workspace ids pointed at the same shared folder must never see each
// other's events.
func TestFolderHubWorkspacesAreIsolated(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "shared")
	newHub := func(ws string) *FolderHub {
		h, err := NewFolderHub(dir, ws, t.TempDir())
		if err != nil {
			t.Fatalf("NewFolderHub(%s): %v", ws, err)
		}
		h.sleep = func(time.Duration) {}
		return h
	}
	hubA, hubB := newHub("ws_alpha"), newHub("ws_beta")
	if err := hubA.Push(ctx, []state.Event{makeEvent("evt_a", "dev_a", 100, 1, "project.added", `{"path":"work/a"}`)}); err != nil {
		t.Fatalf("ws_alpha push: %v", err)
	}
	if err := hubB.Push(ctx, []state.Event{makeEvent("evt_b", "dev_b", 100, 1, "project.added", `{"path":"work/b"}`)}); err != nil {
		t.Fatalf("ws_beta push: %v", err)
	}
	gotA, err := hubA.Pull(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !sameEventIDs(gotA, []string{"evt_a"}) {
		t.Fatalf("ws_alpha pull ids = %v, want only evt_a", ids(gotA))
	}
	gotB, err := hubB.Pull(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !sameEventIDs(gotB, []string{"evt_b"}) {
		t.Fatalf("ws_beta pull ids = %v, want only evt_b", ids(gotB))
	}
}
