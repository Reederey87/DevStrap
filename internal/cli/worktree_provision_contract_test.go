package cli

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
)

// worktreeProvisionKeys is the documented top-level key set for
// worktreeProvisionResult (AD5-01): the pre-existing state.Worktree fields
// plus the additive machine-contract fields. warnings is omitempty and
// intentionally excluded here — it is asserted separately below.
var worktreeProvisionKeys = []string{
	"id", "namespace_id", "device_id", "path", "branch", "base_ref", "base_sha",
	"created_by", "status", "dirty_state",
	"schema_version", "project_path", "remote_url", "default_branch", "repo_path",
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestWorktreeProvisionResultKeySet(t *testing.T) {
	result := worktreeProvisionResult{
		Worktree: state.Worktree{
			ID:          "wt_1",
			NamespaceID: "ns_1",
			DeviceID:    "dev_1",
			Path:        "/code/work/proj-route-tests",
			Branch:      "agent/route-tests",
			BaseRef:     "origin/main",
			BaseSHA:     "deadbeef",
			CreatedBy:   "agent",
			Status:      "active",
			DirtyState:  "clean",
		},
		SchemaVersion: worktreeProvisionSchemaVersion,
		ProjectPath:   "work/proj",
		RemoteURL:     "https://github.com/acme/proj",
		DefaultBranch: "main",
		RepoPath:      "/code/work/proj",
	}

	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if _, ok := decoded["warnings"]; ok {
		t.Fatalf("warnings must be absent when nil, got keys: %v", sortedKeys(decoded))
	}

	want := append([]string{}, worktreeProvisionKeys...)
	sort.Strings(want)
	got := sortedKeys(decoded)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("key set mismatch:\n got  = %v\n want = %v", got, want)
	}
}

func TestWorktreeProvisionResultWarningsPresentWhenSet(t *testing.T) {
	result := worktreeProvisionResult{
		Worktree:      state.Worktree{ID: "wt_1"},
		SchemaVersion: worktreeProvisionSchemaVersion,
		Warnings:      []string{"example warning"},
	}

	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := decoded["warnings"]; !ok {
		t.Fatalf("warnings must be present when set, got keys: %v", sortedKeys(decoded))
	}
}

// TestWorktreeProvisionResultRemoteURLRedaction drives the REAL assembly
// function, not a hand-built struct. Building the struct in the test and
// calling redact.StripURLUserinfo here would only prove that redact works —
// the test would still pass if newWorktreeProvisionResult stopped redacting,
// which is precisely the regression it exists to catch. Mutation-checked:
// drop the redact call in newWorktreeProvisionResult and this fails.
func TestWorktreeProvisionResultRemoteURLRedaction(t *testing.T) {
	const secretToken = "supersecrettoken"
	project := state.ProjectStatus{
		RemoteURL: "https://user:" + secretToken + "@github.com/org/repo.git",
		LocalPath: "/code/work/repo",
	}
	project.Path = "work/repo"

	result := newWorktreeProvisionResult("/code", project, state.Worktree{
		ID:      "wt_1",
		BaseRef: "origin/main",
	})

	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), secretToken) {
		t.Fatalf("secret leaked into worktree new --json output: %s", raw)
	}
	if result.RemoteURL == "" {
		t.Fatal("remote_url was emptied rather than redacted; a harness needs a usable remote")
	}
	if !strings.Contains(result.RemoteURL, "github.com/org/repo.git") {
		t.Fatalf("redaction destroyed the usable part of the URL: %q", result.RemoteURL)
	}
}

// TestWorktreeProvisionResultDerivedFields pins the two derived fields against
// the real assembly: default_branch is cut from BaseRef, and repo_path falls
// back to root+project path only when LocalPath is empty.
func TestWorktreeProvisionResultDerivedFields(t *testing.T) {
	tests := []struct {
		name          string
		localPath     string
		baseRef       string
		wantRepoPath  string
		wantDefBranch string
	}{
		{"local path wins", "/checkout/here", "origin/main", "/checkout/here", "main"},
		{"falls back to root join", "", "origin/develop", filepath.Join("/code", "work", "repo"), "develop"},
		{"branch with slashes keeps remainder", "/x", "origin/release/2.0", "/x", "release/2.0"},
		{"uncuttable base ref leaves branch empty", "/x", "main", "/x", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			project := state.ProjectStatus{LocalPath: tc.localPath}
			project.Path = "work/repo"
			got := newWorktreeProvisionResult("/code", project, state.Worktree{BaseRef: tc.baseRef})
			if got.RepoPath != tc.wantRepoPath {
				t.Errorf("repo_path = %q, want %q", got.RepoPath, tc.wantRepoPath)
			}
			if got.DefaultBranch != tc.wantDefBranch {
				t.Errorf("default_branch = %q, want %q", got.DefaultBranch, tc.wantDefBranch)
			}
			if got.SchemaVersion != worktreeProvisionSchemaVersion {
				t.Errorf("schema_version = %d, want %d", got.SchemaVersion, worktreeProvisionSchemaVersion)
			}
		})
	}
}
