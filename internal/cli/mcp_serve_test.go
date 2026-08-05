package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/viper"
)

// mcpTestOptions builds an *options pointed at home/root the same way
// PersistentPreRunE does (initConfig, then the two flags it would have bound),
// without going through cobra — registerMCPTools takes *options directly and
// the server is never invoked via NewRootCommand's Execute path.
func mcpTestOptions(t *testing.T, home, root string) *options {
	t.Helper()
	opts := &options{v: viper.New()}
	if err := initConfig(opts); err != nil {
		t.Fatalf("initConfig: %v", err)
	}
	opts.v.Set("home", home)
	opts.v.Set("root", root)
	return opts
}

// mcpTestServer wires an in-memory client/server pair around the real tool
// registrations, so the test drives the actual MCP wire protocol (tools/list,
// tools/call) rather than calling Go handler functions directly.
func mcpTestServer(t *testing.T, opts *options) *mcp.ClientSession {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: mcpServerName, Version: "test"}, nil)
	registerMCPTools(server, opts)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// TestMCPDestructiveHintPointersAreNotAliased proves notDestructiveHint
// returns a distinct pointer on every call. A round trip through the wire
// protocol cannot observe this — the client decodes each tool's JSON
// independently into its own fresh *bool regardless of what the server held
// — so this checks the actual registration-time value directly: five
// mcp.ToolAnnotations sharing one *bool would let any mutation through any
// alias flip DestructiveHint for all five tools at once, including the two
// ReadOnlyHint:true ones.
func TestMCPDestructiveHintPointersAreNotAliased(t *testing.T) {
	seen := make(map[*bool]bool)
	for range 5 {
		p := notDestructiveHint()
		if p == nil {
			t.Fatal("notDestructiveHint returned nil")
			return
		}
		if *p {
			t.Fatal("notDestructiveHint returned true, want false")
		}
		if seen[p] {
			t.Fatalf("notDestructiveHint returned the same pointer twice: %p", p)
		}
		seen[p] = true
	}
}

// TestMCPToolsListMatchesDocumentedNames pins the exact five tool names and
// their annotations (AD5-07's design contract) against the real tools/list
// response, not a hand-maintained list — a renamed or dropped tool fails this
// immediately rather than silently breaking an MCP client's tool discovery.
func TestMCPToolsListMatchesDocumentedNames(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	opts := mcpTestOptions(t, home, root)
	session := mcpTestServer(t, opts)

	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	wantReadOnly := map[string]bool{
		"devstrap_worktree_new":    false,
		"devstrap_worktree_adopt":  false,
		"devstrap_worktree_status": true,
		"devstrap_worktree_list":   true,
		"devstrap_agent_adopt":     false,
	}

	var gotNames []string
	for _, tool := range res.Tools {
		gotNames = append(gotNames, tool.Name)
		wantRO, ok := wantReadOnly[tool.Name]
		if !ok {
			continue
		}
		if tool.Annotations == nil {
			t.Errorf("%s: no annotations", tool.Name)
			continue
		}
		if tool.Annotations.ReadOnlyHint != wantRO {
			t.Errorf("%s: ReadOnlyHint = %v, want %v", tool.Name, tool.Annotations.ReadOnlyHint, wantRO)
		}
		if tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
			t.Errorf("%s: DestructiveHint = %v, want a false pointer (none of these five delete anything)", tool.Name, tool.Annotations.DestructiveHint)
		}
		if tool.Description == "" {
			t.Errorf("%s: empty description", tool.Name)
		}
	}

	var wantNames []string
	for name := range wantReadOnly {
		wantNames = append(wantNames, name)
	}
	sort.Strings(gotNames)
	sort.Strings(wantNames)
	if strings.Join(gotNames, ",") != strings.Join(wantNames, ",") {
		t.Fatalf("tool names = %v, want %v", gotNames, wantNames)
	}
}

// TestMCPWorktreeListReachesTheSharedFunction proves devstrap_worktree_list's
// handler reaches the SAME listWorktrees the CLI's `worktree list` calls
// (AD5-07's "no second execution path" acceptance criterion) rather than a
// reimplementation: it seeds a worktree through the real CLI, then asserts
// the MCP tool call sees it with the fields listWorktrees actually returns.
func TestMCPWorktreeListReachesTheSharedFunction(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	setupFreshWorktreeRepo(t, home, root, "auto", false)
	if _, stderr, err := executeForTest("--home", home, "--root", root, "worktree", "new", "work/acme/repo", "--fresh-upstream", "--name", "mcp-list"); err != nil {
		t.Fatalf("worktree new stderr = %q err = %v", stderr, err)
	}

	opts := mcpTestOptions(t, home, root)
	session := mcpTestServer(t, opts)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "devstrap_worktree_list", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("devstrap_worktree_list returned an error result: %+v", result.Content)
	}

	var out mcpWorktreeListResult
	structured, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	if err := json.Unmarshal(structured, &out); err != nil {
		t.Fatalf("unmarshal into mcpWorktreeListResult: %v\n%s", err, structured)
	}
	if out.SchemaVersion != mcpWorktreeListSchemaVersion {
		t.Fatalf("schema_version = %d, want %d", out.SchemaVersion, mcpWorktreeListSchemaVersion)
	}
	if len(out.Worktrees) != 1 {
		t.Fatalf("worktrees = %+v, want exactly 1 (the one created by `worktree new`)", out.Worktrees)
	}
	if !strings.HasPrefix(out.Worktrees[0].Branch, "agent/mcp-list-") {
		t.Fatalf("branch = %q, want the agent/mcp-list-<slug> shape createFreshWorktree derives from --name", out.Worktrees[0].Branch)
	}
}

// TestMCPWorktreeStatusUnknownIDReturnsToolError proves a bad input surfaces
// as a TOOL-level error (IsError=true, a text explanation an agent can act
// on) rather than a raw protocol error — the SDK's default behavior for a
// handler returning a plain Go error, which this test pins so a future change
// to the handler's error path (e.g. wrapping in *jsonrpc.Error) is caught.
func TestMCPWorktreeStatusUnknownIDReturnsToolError(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}

	opts := mcpTestOptions(t, home, root)
	session := mcpTestServer(t, opts)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "devstrap_worktree_status",
		Arguments: map[string]any{"worktree_id": "wt_does_not_exist"},
	})
	if err != nil {
		t.Fatalf("CallTool returned a protocol-level error, want a tool-level one: %v", err)
	}
	if !result.IsError {
		t.Fatalf("result.IsError = false, want true for an unknown worktree id: %+v", result)
	}
	if len(result.Content) == 0 {
		t.Fatal("error result carries no content for the calling agent to read")
	}
}

// TestMCPRequiredFieldsAreEnforcedAtTheSchemaLayer proves the OTHER half of
// required-field enforcement: because every required input field lacks
// `omitempty`, jsonschema-go infers it as a JSON Schema "required" property
// (internal/scan-style inference, confirmed by reading
// google/jsonschema-go's infer.go), so a request that OMITS the key entirely
// never reaches the Go handler at all — the SDK rejects it during argument
// validation with its own message. This is a stronger guarantee than a
// hand-written check (it fails before ANY handler code runs), and it is why
// TestMCPToolsRejectInvalidInputAsToolError below sends an explicit empty
// string rather than omitting the key: that is the only way to reach the
// handler's OWN "X is required" checks, which exist as the second layer for
// exactly that case (a key present but empty).
func TestMCPRequiredFieldsAreEnforcedAtTheSchemaLayer(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	opts := mcpTestOptions(t, home, root)
	session := mcpTestServer(t, opts)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "devstrap_worktree_new", Arguments: map[string]any{"task_name": "t"}})
	if err != nil {
		t.Fatalf("CallTool returned a protocol-level error, want a tool-level one: %v", err)
	}
	if !result.IsError {
		t.Fatalf("result.IsError = false, want true when a required key is omitted entirely")
	}
	text := toolResultText(t, result)
	if !strings.Contains(text, "project_path") {
		t.Fatalf("error text = %q, want it to name the missing property", text)
	}
}

// TestMCPToolsRejectInvalidInputAsToolError table-drives every input
// validation branch mcp_serve.go's HANDLERS actually contain — required
// fields present but empty per tool, agent_adopt's
// allow_shallow-requires-adopt_worktree check, and its non-negative pid
// check — so each is proven to fail closed as a TOOL-level error (an agent
// can read why and retry) rather than either silently succeeding or
// reaching a shared function with a value that function was never asked to
// validate. Every string field is sent as "" rather than omitted: an
// omitted key is caught one layer earlier, at JSON Schema validation (see
// TestMCPRequiredFieldsAreEnforcedAtTheSchemaLayer), and never reaches this
// code at all.
func TestMCPToolsRejectInvalidInputAsToolError(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	opts := mcpTestOptions(t, home, root)

	cases := []struct {
		name      string
		tool      string
		args      map[string]any
		wantError string
	}{
		{"worktree_new empty project_path", "devstrap_worktree_new", map[string]any{"project_path": "", "task_name": "t"}, "project_path is required"},
		{"worktree_new empty task_name", "devstrap_worktree_new", map[string]any{"project_path": "work/x", "task_name": ""}, "task_name is required"},
		{"worktree_adopt empty path", "devstrap_worktree_adopt", map[string]any{"path": ""}, "path is required"},
		{"worktree_status empty worktree_id", "devstrap_worktree_status", map[string]any{"worktree_id": ""}, "worktree_id is required"},
		{"agent_adopt empty arg", "devstrap_agent_adopt", map[string]any{"arg": "", "engine": "e", "task": "t"}, "arg is required"},
		{"agent_adopt empty engine", "devstrap_agent_adopt", map[string]any{"arg": "a", "engine": "", "task": "t"}, "engine is required"},
		{"agent_adopt empty task", "devstrap_agent_adopt", map[string]any{"arg": "a", "engine": "e", "task": ""}, "task is required"},
		{"agent_adopt negative pid", "devstrap_agent_adopt", map[string]any{"arg": "a", "engine": "e", "task": "t", "pid": -1}, "pid must be a positive process id"},
		{"agent_adopt allow_shallow without adopt_worktree", "devstrap_agent_adopt", map[string]any{"arg": "a", "engine": "e", "task": "t", "allow_shallow": true}, "allow_shallow only applies when adopt_worktree"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A fresh in-memory transport pair per case keeps failures isolated;
			// registerMCPTools carries no per-call state, so this is cheap.
			session := mcpTestServer(t, opts)
			result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
			if err != nil {
				t.Fatalf("CallTool returned a protocol-level error, want a tool-level one: %v", err)
			}
			if !result.IsError {
				t.Fatalf("result.IsError = false, want true for invalid input %+v", tc.args)
			}
			text := toolResultText(t, result)
			if !strings.Contains(text, tc.wantError) {
				t.Fatalf("error text = %q, want it to contain %q", text, tc.wantError)
			}
		})
	}
}

func toolResultText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	var b strings.Builder
	for _, c := range result.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// TestMCPServerNeverWritesToStdout is the concrete stdio-hygiene risk this
// server design calls out: the reused business-logic functions take an
// io.Writer parameter for CLI progress/warnings, and every call site in
// mcp_serve.go must pass io.Discard rather than any writer touching os.Stdout
// — a stray byte on stdout corrupts the JSON-RPC stream for every other tool
// call on the same connection. This test cannot observe os.Stdout directly
// without capturing the process-wide fd, so it instead pins the discipline at
// the source: every writer argument passed to createFreshWorktree,
// adoptWorktreeAt, and adoptAgentRun in this file is the literal io.Discard.
func TestMCPServerNeverWritesToStdout(t *testing.T) {
	raw, err := os.ReadFile("mcp_serve.go")
	if err != nil {
		t.Fatalf("read mcp_serve.go: %v", err)
	}
	src := string(raw)
	for _, call := range []string{
		"createFreshWorktree(ctx, io.Discard, io.Discard,",
		"adoptWorktreeAt(ctx, io.Discard,",
		"adoptAgentRun(ctx, io.Discard,",
	} {
		if !strings.Contains(src, call) {
			t.Errorf("expected call shape not found (writer arg no longer io.Discard?): %q", call)
		}
	}
}
