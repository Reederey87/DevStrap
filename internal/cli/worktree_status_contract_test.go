package cli

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// worktreeStatusKeys is the documented top-level key set for
// worktreeStatusOutput, the `worktree status --json` machine contract
// (spec/13 § Machine contract surfaces). Unlike worktreeProvisionResult this
// struct embeds nothing, so every key below is declared on it directly. No
// field is omitempty, so the whole set is always present — including
// behind: 0 and fresh: false, which a consumer must be able to read without
// having to treat an absent key as a default.
var worktreeStatusKeys = []string{
	"schema_version", "id", "path", "branch", "base_ref", "base_sha",
	"current_sha", "fresh", "behind", "dirty_state",
}

func TestWorktreeStatusOutputKeySet(t *testing.T) {
	out := worktreeStatusOutput{
		SchemaVersion: worktreeStatusSchemaVersion,
		ID:            "wt_1",
		Path:          "/code/work/proj-route-tests",
		Branch:        "agent/route-tests",
		BaseRef:       "origin/main",
		BaseSHA:       "deadbeef",
		CurrentSHA:    "cafebabe",
		Fresh:         false,
		Behind:        3,
		DirtyState:    "clean",
	}

	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	want := append([]string{}, worktreeStatusKeys...)
	sort.Strings(want)
	got := sortedKeys(decoded)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("key set mismatch:\n got  = %v\n want = %v", got, want)
	}
}

// TestWorktreeStatusOutputZeroValueKeysPresent pins the property the key-set
// test above cannot: that the contract survives the ZERO value. An
// `omitempty` added to fresh, behind, or dirty_state would keep that test
// green — it marshals a fully-populated struct — while silently dropping keys
// from every clean, fresh worktree, which is the common case. A consumer
// reading `behind` would start seeing "missing" instead of 0.
func TestWorktreeStatusOutputZeroValueKeysPresent(t *testing.T) {
	raw, err := json.Marshal(worktreeStatusOutput{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := append([]string{}, worktreeStatusKeys...)
	sort.Strings(want)
	got := sortedKeys(decoded)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("zero-value key set mismatch (an omitempty was added?):\n got  = %v\n want = %v", got, want)
	}
}

// TestWorktreeStatusJSONContractEndToEnd drives the REAL command against a
// real repo, so it fails if statusWorktree stops stamping schema_version or
// if the rendered document ever stops being the bare worktreeStatusOutput
// object. Asserting the key set on a hand-built struct (above) cannot catch
// either: it proves what the type marshals to, not what the command emits.
func TestWorktreeStatusJSONContractEndToEnd(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	setupFreshWorktreeRepo(t, home, root, "auto", false)

	if _, stderr, err := executeForTest("--home", home, "--root", root, "worktree", "new", "work/acme/repo", "--fresh-upstream", "--name", "contract"); err != nil {
		t.Fatalf("worktree new stderr = %q err = %v", stderr, err)
	}
	worktrees := listWorktreesForTest(t, home, root)
	if len(worktrees) != 1 {
		t.Fatalf("worktree count = %d, want 1: %+v", len(worktrees), worktrees)
	}

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json", "worktree", "status", worktrees[0].ID)
	if err != nil {
		t.Fatalf("worktree status stderr = %q err = %v", stderr, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("worktree status --json is not a bare object: %v\n%s", err, stdout)
	}
	if got := decoded["schema_version"]; got != float64(worktreeStatusSchemaVersion) {
		t.Fatalf("schema_version = %v, want %d", got, worktreeStatusSchemaVersion)
	}
	want := append([]string{}, worktreeStatusKeys...)
	sort.Strings(want)
	if got := sortedKeys(decoded); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("emitted key set mismatch:\n got  = %v\n want = %v", got, want)
	}
}

// TestWorktreeListJSONIsBareArray guards the AD5-07 design decision that
// `worktree list --json` stays a bare JSON array of state.Worktree objects.
// listWorktrees returns []state.Worktree precisely so it cannot drift into a
// versioned envelope: wrapping a top-level array in an object is a BREAKING
// shape change for existing consumers, not the additive evolution spec/13
// § Machine contract surfaces permits, so it can never be done as a
// consistency tidy-up alongside the other schema_version work. An MCP tool
// built on listWorktrees may wrap the slice for its own separate consumer;
// this surface may not. The existing txtar coverage greps for a key inside
// the payload, which an envelope would not disturb — hence this test.
func TestWorktreeListJSONIsBareArray(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	setupFreshWorktreeRepo(t, home, root, "auto", false)

	if _, stderr, err := executeForTest("--home", home, "--root", root, "worktree", "new", "work/acme/repo", "--fresh-upstream", "--name", "list-shape"); err != nil {
		t.Fatalf("worktree new stderr = %q err = %v", stderr, err)
	}

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json", "worktree", "list")
	if err != nil {
		t.Fatalf("worktree list stderr = %q err = %v", stderr, err)
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(stdout), &arr); err != nil {
		t.Fatalf("worktree list --json is not a bare array (an envelope was added?): %v\n%s", err, stdout)
	}
	if len(arr) != 1 {
		t.Fatalf("worktree list --json len = %d, want 1: %s", len(arr), stdout)
	}
	if _, ok := arr[0]["created_by"]; !ok {
		t.Fatalf("array element is not a state.Worktree object: %s", stdout)
	}
}

// TestWorktreeListJSONEmptyIsNull pins what the zero-worktree case ACTUALLY
// emits — `null`, not `[]` — because state.Store.ListWorktrees returns a nil
// slice and encoding/json renders nil as null. This test blesses nothing: it
// exists so the shape is written down, since the sibling test above covers
// only the populated case and would let a reader conclude the surface is
// always an array. It is not, and a consumer doing `for x of JSON.parse(out)`
// crashes on a fresh workspace.
//
// Normalizing nil to an empty slice is the obvious fix and is deliberately
// NOT done here: this PR's acceptance bar is that no CLI output changes, and
// `null` -> `[]` is an output change that deserves its own decision rather
// than riding along in a refactor. When that decision is taken, this test is
// the one to flip.
func TestWorktreeListJSONEmptyIsNull(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json", "worktree", "list")
	if err != nil {
		t.Fatalf("worktree list stderr = %q err = %v", stderr, err)
	}
	if strings.TrimSpace(stdout) != "null" {
		t.Fatalf("empty worktree list --json = %q, want \"null\" (see this test's comment before changing it)", stdout)
	}
	// Whatever the shape, it must still decode as a (possibly nil) array
	// rather than an object — the no-envelope invariant holds for the empty
	// case too.
	var arr []map[string]any
	if err := json.Unmarshal([]byte(stdout), &arr); err != nil {
		t.Fatalf("empty worktree list --json does not decode as an array: %v\n%s", err, stdout)
	}
	if len(arr) != 0 {
		t.Fatalf("empty worktree list --json len = %d, want 0", len(arr))
	}
}
