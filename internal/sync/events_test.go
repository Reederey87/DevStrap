package sync

import (
	"strings"
	"testing"
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
