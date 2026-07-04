package hub

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
)

// newBareCarrier creates a local bare repository to act as the carrier remote.
func newBareCarrier(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "hub.git")
	cmd := exec.Command("git", "init", "--quiet", "--bare", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("init bare carrier: %v: %s", err, out)
	}
	return dir
}

// TestGitCarrierSmokeRoundTrip pushes events and a blob from one device's
// carrier hub and reads them back from a second, independently cloned
// instance — the minimal two-device convergence path, including the
// empty-remote bootstrap, dedup re-push, and the missing-blob error contract.
func TestGitCarrierSmokeRoundTrip(t *testing.T) {
	ctx := context.Background()
	remote := newBareCarrier(t)

	hubA, err := NewGitCarrierHub(remote, "main", "ws_test", filepath.Join(t.TempDir(), "a"))
	if err != nil {
		t.Fatalf("new hub A: %v", err)
	}
	hubB, err := NewGitCarrierHub(remote, "main", "ws_test", filepath.Join(t.TempDir(), "b"))
	if err != nil {
		t.Fatalf("new hub B: %v", err)
	}

	// Push two events from A (bootstrap: creates the branch + marker).
	if err := hubA.Push(ctx, []state.Event{
		makeEvent("evt_a1", "dev_a", 100, 1, "project.added", `{"path":"work/x"}`),
		makeEvent("evt_a2", "dev_a", 200, 2, "project.added", `{"path":"work/y"}`),
	}); err != nil {
		t.Fatalf("push A: %v", err)
	}

	// B pulls from a cold clone.
	got, err := hubB.Pull(ctx, dssync.Cursor{})
	if err != nil {
		t.Fatalf("pull B: %v", err)
	}
	if len(got) != 2 || got[0].ID != "evt_a1" || got[1].ID != "evt_a2" {
		t.Fatalf("pull B = %+v, want evt_a1, evt_a2", got)
	}

	// Idempotent re-push (conditional-put dedup) must be a clean no-op.
	if err := hubA.Push(ctx, []state.Event{
		makeEvent("evt_a1", "dev_a", 100, 1, "project.added", `{"path":"work/x"}`),
	}); err != nil {
		t.Fatalf("dedup re-push: %v", err)
	}

	// Blob round trip A -> B.
	const blobSHA = "aa00000000000000000000000000000000000000000000000000000000000001"
	if err := hubA.PutBlob(ctx, blobSHA, strings.NewReader("ciphertext")); err != nil {
		t.Fatalf("put blob: %v", err)
	}
	rc, err := hubB.GetBlob(ctx, blobSHA)
	if err != nil {
		t.Fatalf("get blob: %v", err)
	}
	data, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(data) != "ciphertext" {
		t.Fatalf("blob content = %q, err %v", data, err)
	}

	// Missing blob is ErrBlobNotFound.
	if _, err := hubB.GetBlob(ctx, strings.Repeat("bb", 32)); !errors.Is(err, dssync.ErrBlobNotFound) {
		t.Fatalf("missing blob err = %v, want ErrBlobNotFound", err)
	}
}
