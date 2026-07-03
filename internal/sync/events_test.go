package sync

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
)

// TestNewProjectEventStripsRemoteCredentials covers ENV-2/SEC-3: a
// credential-bearing remote URL must never be persisted into an event payload.
func TestNewProjectEventStripsRemoteCredentials(t *testing.T) {
	// Built at runtime so no contiguous secret literal is committed.
	token := "ghp_" + "supersecrettoken1234567890ABCDEF"
	event, err := NewProjectEvent("dev_test", EventProjectAdded, 1, ProjectPayload{
		Path:      "work/acme/api",
		Type:      "git_repo",
		RemoteURL: "https://x-access-token:" + token + "@github.com/acme/api.git",
		RemoteKey: "github.com/acme/api",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(event.PayloadJSON, token) {
		t.Fatalf("token leaked into event payload: %s", event.PayloadJSON)
	}
	// The host/path must survive so the receiving device can still hydrate.
	if !strings.Contains(event.PayloadJSON, "github.com/acme/api.git") {
		t.Fatalf("remote host/path was lost: %s", event.PayloadJSON)
	}
}

func TestCreateProjectEventTxRollsBackWithTx(t *testing.T) {
	ctx := context.Background()
	st, err := state.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(ctx, "test", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureDevice(ctx, "device-a"); err != nil {
		t.Fatal(err)
	}
	rollbackErr := errors.New("force rollback")
	err = st.WithTx(ctx, func(tx *state.Tx) error {
		if _, err := CreateProjectEventTx(ctx, st, tx, EventProjectAdded, ProjectPayload{
			Path:          "work/acme/api",
			Type:          "git_repo",
			RemoteURL:     "git@github.com:acme/api.git",
			RemoteKey:     "github.com/acme/api",
			DefaultBranch: "main",
		}); err != nil {
			return err
		}
		return rollbackErr
	})
	if !errors.Is(err, rollbackErr) {
		t.Fatalf("WithTx err = %v, want rollbackErr", err)
	}
	events, err := st.PendingEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("events persisted after rollback: %+v", events)
	}
}
