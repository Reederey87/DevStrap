package cli

import (
	"encoding/json"
	"path/filepath"
	"reflect"
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

// TestWorktreeProvisionResultNoTagShadowing guards a silent failure mode of Go's
// embedded-struct JSON promotion: when an outer field and an embedded field carry
// the SAME json tag, the shallower one wins and the deeper one is dropped from the
// output with no error, no warning, and no compile failure. Nothing collides today,
// but if state.Worktree ever gains a field tagged `warnings` or `schema_version`,
// that field would silently vanish from `worktree new --json` — a machine contract
// losing a key without anything failing. This test turns that into a build break.
func TestWorktreeProvisionResultNoTagShadowing(t *testing.T) {
	jsonTags := func(typ reflect.Type) map[string]bool {
		out := map[string]bool{}
		for i := range typ.NumField() {
			tag, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
			if tag != "" && tag != "-" {
				out[tag] = true
			}
		}
		return out
	}

	inner := jsonTags(reflect.TypeOf(state.Worktree{}))
	outer := reflect.TypeOf(worktreeProvisionResult{})
	for i := range outer.NumField() {
		f := outer.Field(i)
		if f.Anonymous {
			continue // the state.Worktree embed itself
		}
		tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if tag == "" || tag == "-" {
			continue
		}
		if inner[tag] {
			t.Errorf("json tag %q is declared on BOTH worktreeProvisionResult.%s and state.Worktree; "+
				"Go silently drops the embedded field, so the contract would lose a key with no error. "+
				"Rename one of them.", tag, f.Name)
		}
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

// TestWorktreeProvisionResultRemoteURLShapes pins what redaction does to each
// remote shape DevStrap actually accepts, including the one that is NOT parsed.
// The scp-like form is deliberately passed through unchanged: url.Parse rejects
// it ("first path segment in URL cannot contain colon"), so StripURLUserinfo
// returns it verbatim. That is safe rather than merely tolerated — git's scp-like
// syntax is [user@]host:path and has no password-embedding mechanism at all, so
// the only thing that could ride through is the SSH login name, which the ssh://
// branch deliberately preserves too. This test exists so a future reader does not
// assume every shape is parsed, and so a change to redact that DID start parsing
// scp-like remotes has to come past an explicit expectation.
func TestWorktreeProvisionResultRemoteURLShapes(t *testing.T) {
	tests := []struct {
		name   string
		remote string
		want   string
	}{
		{"scp-like ssh passes through unchanged", "git@github.com:org/repo.git", "git@github.com:org/repo.git"},
		{"https userinfo fully dropped", "https://user:tok@github.com/o/r.git", "https://github.com/o/r.git"},
		{"https bare token dropped", "https://tok@github.com/o/r.git", "https://github.com/o/r.git"},
		{"ssh scheme keeps login, drops password", "ssh://git:tok@github.com/o/r.git", "ssh://git@github.com/o/r.git"},
		{"no userinfo untouched", "https://github.com/o/r.git", "https://github.com/o/r.git"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := newWorktreeProvisionResult("/code", state.ProjectStatus{RemoteURL: tc.remote}, state.Worktree{})
			if got.RemoteURL != tc.want {
				t.Errorf("remote_url = %q, want %q", got.RemoteURL, tc.want)
			}
		})
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
