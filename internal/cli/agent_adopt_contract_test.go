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

// agentAdoptKeys is the documented top-level key set for agentAdoptResult
// (AD5-03): the pre-existing state.AgentRun fields plus the additive
// machine-contract fields. Every state.AgentRun field except id, namespace_id,
// engine, task and status is omitempty, so this is the set emitted by a run
// carrying every field — warnings is asserted separately, as in the sibling
// worktreeProvisionResult test.
var agentAdoptKeys = []string{
	"id", "namespace_id", "worktree_id", "engine", "task", "policy_id",
	"status", "base_ref", "base_sha", "branch", "log_path", "diff_summary",
	"test_summary", "runner_pid", "runner_started_at", "sandbox_backend",
	"sandbox_mode", "sandbox_limitations",
	"schema_version", "worktree_path",
}

func TestAgentAdoptResultKeySet(t *testing.T) {
	result := agentAdoptResult{
		AgentRun: state.AgentRun{
			ID:                 "arun_1",
			NamespaceID:        "ns_1",
			WorktreeID:         "wt_1",
			Engine:             "claude-code",
			Task:               "route tests",
			PolicyID:           "pol_1",
			Status:             "running",
			BaseRef:            "origin/main",
			BaseSHA:            "deadbeef",
			Branch:             "agent/route-tests",
			LogPath:            "/logs/arun_1.log",
			DiffSummary:        "1 file changed",
			TestSummary:        "go test ./... ok",
			RunnerPID:          4242,
			RunnerStartedAt:    1700000000,
			SandboxBackend:     "seatbelt",
			SandboxMode:        "enforced",
			SandboxLimitations: `["no-network"]`,
		},
		SchemaVersion: agentAdoptSchemaVersion,
		WorktreePath:  "/code/work/proj-route-tests",
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

	want := append([]string{}, agentAdoptKeys...)
	sort.Strings(want)
	got := sortedKeys(decoded)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("key set mismatch:\n got  = %v\n want = %v", got, want)
	}
}

// TestAgentAdoptResultNoTagShadowing guards the same silent failure mode
// TestWorktreeProvisionResultNoTagShadowing guards for the provision
// contract: when an outer field and an embedded field carry the SAME json
// tag, Go's promotion rules drop the deeper one with no error, no warning and
// no compile failure. Nothing collides today, but a state.AgentRun field
// tagged `schema_version`, `worktree_path` or `warnings` would silently
// vanish from `agent adopt --json`. This turns that into a test failure.
func TestAgentAdoptResultNoTagShadowing(t *testing.T) {
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

	inner := jsonTags(reflect.TypeOf(state.AgentRun{}))
	outer := reflect.TypeOf(agentAdoptResult{})
	for i := range outer.NumField() {
		f := outer.Field(i)
		if f.Anonymous {
			continue // the state.AgentRun embed itself
		}
		tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if tag == "" || tag == "-" {
			continue
		}
		if inner[tag] {
			t.Errorf("json tag %q is declared on BOTH agentAdoptResult.%s and state.AgentRun; "+
				"Go silently drops the embedded field, so the contract would lose a key with no error. "+
				"Rename one of them.", tag, f.Name)
		}
	}
}

// TestAgentAdoptJSONStampsSchemaVersion drives the REAL command, because the
// key-set test above provably cannot catch a dropped stamp: it hand-builds the
// struct with SchemaVersion already set, and since the field is not omitempty
// the key is present even at value 0 — so the key-set comparison passes
// whatever adoptAgentRun actually returns. Mutation-checked: deleting
// `SchemaVersion: agentAdoptSchemaVersion` from adoptAgentRun's return left
// the entire suite green before this test existed.
func TestAgentAdoptJSONStampsSchemaVersion(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	localPath := setupFreshWorktreeRepo(t, home, root, "auto", false)
	head := strings.TrimSpace(runGitOutput(t, localPath, "rev-parse", "HEAD"))
	extWT := filepath.Join(t.TempDir(), "external-wt")
	runGit(t, localPath, "worktree", "add", "--detach", extWT, head)

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "agent", "adopt", extWT,
		"--engine", "claude-code", "--task", "schema version", "--adopt-worktree", "--json")
	if err != nil {
		t.Fatalf("agent adopt stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("agent adopt --json is not a bare object: %v\n%s", err, stdout)
	}
	if got := decoded["schema_version"]; got != float64(agentAdoptSchemaVersion) {
		t.Fatalf("schema_version = %v, want %d", got, agentAdoptSchemaVersion)
	}
	if got := decoded["worktree_path"]; got == nil || got == "" {
		t.Fatalf("worktree_path missing from emitted payload: %s", stdout)
	}
}

func TestAgentAdoptResultWarningsPresentWhenSet(t *testing.T) {
	result := agentAdoptResult{
		AgentRun:      state.AgentRun{ID: "arun_1"},
		SchemaVersion: agentAdoptSchemaVersion,
		Warnings:      []string{"repository is a shallow clone"},
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
