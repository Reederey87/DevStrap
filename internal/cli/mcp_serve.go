package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

// mcpServerName is the MCP Implementation name advertised to clients and is
// also the tool-name prefix (AD5-07's decision record): a server loaded
// alongside three to five others makes a bare `worktree_new` a name another
// tool will also want.
const mcpServerName = "devstrap"

func newMCPCommand(opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run devstrap as a local MCP server",
	}
	cmd.AddCommand(newMCPServeCommand(opts))
	return cmd
}

func newMCPServeCommand(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Serve the worktree/agent provisioning tools over stdio MCP",
		Long: "Serve the worktree/agent provisioning tools over stdio MCP (AD5-07).\n\n" +
			"Exposes five tools — devstrap_worktree_new, devstrap_worktree_adopt,\n" +
			"devstrap_worktree_status, devstrap_worktree_list, and\n" +
			"devstrap_agent_adopt — each calling the exact same internal Go function\n" +
			"its cobra command calls. There is no second execution path: a fresh\n" +
			"worktree still comes from a fetched origin/<default_branch> with a\n" +
			"recorded base SHA, and adoption still never rewrites a base it did not\n" +
			"resolve itself.\n\n" +
			"The local stdio subprocess boundary IS the trust boundary — there is no\n" +
			"authentication, matching the precedent of `docker agent serve mcp` and\n" +
			"`container-use stdio`. Add it to an MCP client with:\n\n" +
			"  claude mcp add devstrap -- devstrap mcp serve",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMCPServe(cmd.Context(), opts)
		},
	}
}

func runMCPServe(ctx context.Context, opts *options) error {
	server := mcp.NewServer(&mcp.Implementation{Name: mcpServerName, Version: version}, nil)
	registerMCPTools(server, opts)
	return server.Run(ctx, &mcp.StdioTransport{})
}

// notDestructiveHint returns a fresh pointer for each call. Every tool below
// is DestructiveHint: false, but each mcp.ToolAnnotations gets its OWN *bool
// rather than one shared cell — a single shared pointer would let a mutation
// through any alias (SDK internals, a future edit, reflection) flip the hint
// for all five tools, including the two ReadOnlyHint: true ones, at once.
func notDestructiveHint() *bool {
	v := false
	return &v
}

func registerMCPTools(server *mcp.Server, opts *options) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "devstrap_worktree_new",
		Title:       "Create a fresh worktree",
		Description: "Create a fresh git worktree for an already-registered project, based on a freshly fetched origin/<default_branch> with a recorded base SHA. Fails if the project path is not already registered — adopt it first with devstrap_worktree_adopt, or via the CLI's `scan --adopt`.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: notDestructiveHint()},
	}, mcpWorktreeNew(opts))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "devstrap_worktree_adopt",
		Title:       "Adopt an externally-created worktree",
		Description: "Register a linked git worktree that a harness (Claude Code, Cursor, Codex, Devin) already created itself — e.g. via a plain `git worktree add` — so DevStrap's stale-base gate and provenance registry apply to it. Records what the worktree was ACTUALLY based on; never rewrites, repairs, or blesses that base. Detached HEAD is adopted as the common case, not refused.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: notDestructiveHint()},
	}, mcpWorktreeAdopt(opts))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "devstrap_worktree_status",
		Title:       "Check worktree freshness",
		Description: "Report whether a registered worktree is fresh or stale against its recorded base ref, how many commits behind, and its current dirty state.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: notDestructiveHint()},
	}, mcpWorktreeStatus(opts))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "devstrap_worktree_list",
		Title:       "List registered worktrees",
		Description: "List every worktree DevStrap has registered for this workspace, including ones adopted from an externally-created checkout (created_by=\"adopted\") alongside ones DevStrap itself provisioned.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: notDestructiveHint()},
	}, mcpWorktreeList(opts))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "devstrap_agent_adopt",
		Title:       "Register an agent run",
		Description: "Register an agent run against a worktree a real harness already created and ran in, optionally adopting that worktree first (adopt_worktree=true). Required to later call the CLI's `agent pr` under the same stale-base gate as a DevStrap-provisioned run.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: notDestructiveHint()},
	}, mcpAgentAdopt(opts))
}

// --- devstrap_worktree_new ---

type mcpWorktreeNewInput struct {
	ProjectPath string `json:"project_path" jsonschema:"namespace path of the already-registered project, e.g. work/acme/api-server"`
	TaskName    string `json:"task_name" jsonschema:"short slug used in the created branch name"`
}

func mcpWorktreeNew(opts *options) mcp.ToolHandlerFor[mcpWorktreeNewInput, worktreeProvisionResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in mcpWorktreeNewInput) (*mcp.CallToolResult, worktreeProvisionResult, error) {
		if in.ProjectPath == "" {
			return nil, worktreeProvisionResult{}, fmt.Errorf("project_path is required")
		}
		if in.TaskName == "" {
			return nil, worktreeProvisionResult{}, fmt.Errorf("task_name is required")
		}
		store, err := opts.openState(ctx)
		if err != nil {
			return nil, worktreeProvisionResult{}, err
		}
		defer closeStore(store)
		project, err := store.ProjectByPath(ctx, in.ProjectPath)
		if err != nil {
			return nil, worktreeProvisionResult{}, err
		}
		// io.Discard for both writers: their non-fatal-warning text has no
		// structured equivalent on worktreeProvisionResult today (the CLI's own
		// `worktree new --json` already loses it — Warnings is declared but never
		// populated by newWorktreeProvisionResult), so this tool sees exactly what
		// a `--json` CLI consumer already sees, not less.
		wt, err := createFreshWorktree(ctx, io.Discard, io.Discard, opts, store, project, in.TaskName, "mcp")
		if err != nil {
			return nil, worktreeProvisionResult{}, err
		}
		return nil, newWorktreeProvisionResult(opts.paths().Root, project, wt), nil
	}
}

// --- devstrap_worktree_adopt ---

type mcpWorktreeAdoptInput struct {
	Path         string `json:"path" jsonschema:"absolute path to the linked worktree to adopt"`
	Project      string `json:"project,omitempty" jsonschema:"namespace path of the owning project; only needed when it cannot be inferred uniquely from the worktree's main checkout"`
	BaseRef      string `json:"base_ref,omitempty" jsonschema:"explicit base ref such as origin/gh-pages, instead of the resolved default branch"`
	AllowShallow bool   `json:"allow_shallow,omitempty" jsonschema:"adopt even though the repository is a shallow clone (recorded base_sha may be inaccurate)"`
}

func mcpWorktreeAdopt(opts *options) mcp.ToolHandlerFor[mcpWorktreeAdoptInput, worktreeAdoptResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in mcpWorktreeAdoptInput) (*mcp.CallToolResult, worktreeAdoptResult, error) {
		if in.Path == "" {
			return nil, worktreeAdoptResult{}, fmt.Errorf("path is required")
		}
		store, err := opts.openState(ctx)
		if err != nil {
			return nil, worktreeAdoptResult{}, err
		}
		defer closeStore(store)
		wt, project, outcome, err := adoptWorktreeAt(ctx, io.Discard, opts, store, in.Path, in.Project, in.BaseRef, in.AllowShallow)
		if err != nil {
			return nil, worktreeAdoptResult{}, err
		}
		return nil, worktreeAdoptResult{
			Worktree:          wt,
			ProjectPath:       project.Path,
			AlreadyAdopted:    outcome.AlreadyAdopted,
			AlreadyRegistered: outcome.AlreadyRegistered,
			Warnings:          outcome.Warnings,
		}, nil
	}
}

// --- devstrap_worktree_status ---

type mcpWorktreeStatusInput struct {
	WorktreeID string `json:"worktree_id" jsonschema:"the worktree's id, as returned by devstrap_worktree_new or devstrap_worktree_list"`
}

func mcpWorktreeStatus(opts *options) mcp.ToolHandlerFor[mcpWorktreeStatusInput, worktreeStatusOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in mcpWorktreeStatusInput) (*mcp.CallToolResult, worktreeStatusOutput, error) {
		if in.WorktreeID == "" {
			return nil, worktreeStatusOutput{}, fmt.Errorf("worktree_id is required")
		}
		store, err := opts.openState(ctx)
		if err != nil {
			return nil, worktreeStatusOutput{}, err
		}
		defer closeStore(store)
		out, err := statusWorktree(ctx, opts, store, in.WorktreeID)
		if err != nil {
			return nil, worktreeStatusOutput{}, err
		}
		return nil, out, nil
	}
}

// --- devstrap_worktree_list ---

// mcpWorktreeListResult wraps listWorktrees' bare []state.Worktree in a
// versioned envelope for MCP consumers. This is a SEPARATE contract from
// `worktree list --json`, which stays a top-level array on purpose (AD5-07 PR
// A): wrapping the CLI's own output would be a breaking shape change for
// every existing consumer of that flag. A tool call is a different consumer
// with no such history, so it is free to start versioned.
type mcpWorktreeListResult struct {
	SchemaVersion int              `json:"schema_version"`
	Worktrees     []state.Worktree `json:"worktrees"`
}

const mcpWorktreeListSchemaVersion = 1

type mcpWorktreeListInput struct{}

func mcpWorktreeList(opts *options) mcp.ToolHandlerFor[mcpWorktreeListInput, mcpWorktreeListResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ mcpWorktreeListInput) (*mcp.CallToolResult, mcpWorktreeListResult, error) {
		store, err := opts.openState(ctx)
		if err != nil {
			return nil, mcpWorktreeListResult{}, err
		}
		defer closeStore(store)
		worktrees, err := listWorktrees(ctx, store)
		if err != nil {
			return nil, mcpWorktreeListResult{}, err
		}
		return nil, mcpWorktreeListResult{SchemaVersion: mcpWorktreeListSchemaVersion, Worktrees: worktrees}, nil
	}
}

// --- devstrap_agent_adopt ---

type mcpAgentAdoptInput struct {
	Arg           string `json:"arg" jsonschema:"a worktrees.id, or the filesystem path of the worktree to adopt when adopt_worktree is true"`
	Engine        string `json:"engine" jsonschema:"the harness name, e.g. claude-code, cursor, codex"`
	Task          string `json:"task" jsonschema:"short human-readable description of what the agent is doing"`
	LogPath       string `json:"log_path,omitempty" jsonschema:"path to the harness's own session log, recorded for provenance"`
	Project       string `json:"project,omitempty" jsonschema:"namespace path of the owning project; only needed when adopt_worktree is true and it cannot be inferred uniquely"`
	BaseRef       string `json:"base_ref,omitempty" jsonschema:"explicit base ref instead of the resolved default branch; only used when adopt_worktree is true"`
	PID           int    `json:"pid,omitempty" jsonschema:"the harness's own process id, if known; enables the dead-PID interrupted sweep. Omit or send 0 when unknown — never guess a PID"`
	AdoptWorktree bool   `json:"adopt_worktree,omitempty" jsonschema:"if true, arg is a filesystem path and is adopted as a worktree first via the same path devstrap_worktree_adopt uses"`
	AllowShallow  bool   `json:"allow_shallow,omitempty" jsonschema:"adopt even though the repository is a shallow clone; only used when adopt_worktree is true"`
}

func mcpAgentAdopt(opts *options) mcp.ToolHandlerFor[mcpAgentAdoptInput, agentAdoptResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in mcpAgentAdoptInput) (*mcp.CallToolResult, agentAdoptResult, error) {
		if in.Arg == "" {
			return nil, agentAdoptResult{}, fmt.Errorf("arg is required")
		}
		if in.Engine == "" {
			return nil, agentAdoptResult{}, fmt.Errorf("engine is required")
		}
		if in.Task == "" {
			return nil, agentAdoptResult{}, fmt.Errorf("task is required")
		}
		if in.PID < 0 {
			return nil, agentAdoptResult{}, fmt.Errorf("pid must be a positive process id, or 0 when unknown; got %d", in.PID)
		}
		if in.AllowShallow && !in.AdoptWorktree {
			return nil, agentAdoptResult{}, fmt.Errorf("allow_shallow only applies when adopt_worktree registers the worktree; the worktree named by arg is already registered, so its base was recorded when it was adopted")
		}
		store, err := opts.openState(ctx)
		if err != nil {
			return nil, agentAdoptResult{}, err
		}
		defer closeStore(store)
		out, err := adoptAgentRun(ctx, io.Discard, opts, store, in.Arg, in.Engine, in.Task, in.LogPath, in.Project, in.BaseRef, in.PID, in.AdoptWorktree, in.AllowShallow)
		if err != nil {
			return nil, agentAdoptResult{}, err
		}
		return nil, out, nil
	}
}
