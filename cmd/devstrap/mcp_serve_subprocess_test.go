package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMCPServeRealSubprocess drives a REAL devstrap binary — built fresh, not
// the testscript re-exec trick — as an MCP client would: spawn it, speak the
// wire protocol over its actual os.Stdin/os.Stdout, and call a real tool.
//
// The in-process tests in internal/cli exercise the real wire protocol too,
// but over an in-memory transport, so they cannot see two things this test
// exists for: that `devstrap mcp serve`'s RunE wiring actually connects
// StdioTransport to the process's real stdio, and that NOTHING on the whole
// startup path — cobra flag parsing, viper config loading, logging init, not
// just the tool handlers already checked in internal/cli — writes a stray
// byte to stdout before the session starts. A single such byte corrupts the
// JSON-RPC stream for every tool call on the connection.
func TestMCPServeRealSubprocess(t *testing.T) {
	bin := buildDevstrapBinary(t)

	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	initDevstrapWorkspace(t, bin, home, root)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "--home", home, "--root", root, "mcp", "serve")
	client := mcp.NewClient(&mcp.Implementation{Name: "subprocess-test", Version: "test"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect to real devstrap mcp serve subprocess: %v", err)
	}
	defer func() { _ = session.Close() }()

	listRes, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list against the real subprocess: %v", err)
	}
	var gotNames []string
	for _, tool := range listRes.Tools {
		gotNames = append(gotNames, tool.Name)
	}
	sort.Strings(gotNames)
	want := []string{
		"devstrap_agent_adopt",
		"devstrap_worktree_adopt",
		"devstrap_worktree_list",
		"devstrap_worktree_new",
		"devstrap_worktree_status",
	}
	if strings.Join(gotNames, ",") != strings.Join(want, ",") {
		t.Fatalf("tool names from the real subprocess = %v, want %v", gotNames, want)
	}

	callRes, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "devstrap_worktree_list", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("CallTool devstrap_worktree_list against the real subprocess: %v", err)
	}
	if callRes.IsError {
		t.Fatalf("devstrap_worktree_list returned a tool error against a freshly initialized (zero-worktree) workspace: %+v", callRes.Content)
	}
}

func buildDevstrapBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "devstrap-mcp-subprocess-test")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build devstrap: %v\n%s", err, out)
	}
	return bin
}

func initDevstrapWorkspace(t *testing.T, bin, home, root string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "--home", home, "--root", root, "init")
	cmd.Env = append(os.Environ(), "DEVSTRAP_NO_KEYCHAIN=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("devstrap init: %v\n%s", err, out)
	}
}
