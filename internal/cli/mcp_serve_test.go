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
