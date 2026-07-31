package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/Reederey87/DevStrap/internal/id"
	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/cobra"
)

// agentAdoptResult is the --json payload for `agent adopt` (AD5-03). It
// embeds state.AgentRun the same way worktreeProvisionResult embeds
// state.Worktree, so every field the store already exposes keeps its exact
// name and position — an additive extension, not a reshape.
type agentAdoptResult struct {
	state.AgentRun
	SchemaVersion int      `json:"schema_version"`
	WorktreePath  string   `json:"worktree_path"`
	Warnings      []string `json:"warnings,omitempty"`
}

// agentAdoptSchemaVersion is the contract version for agentAdoptResult. Bump
// ONLY for an additive change.
const agentAdoptSchemaVersion = 1

// agentFinishResult is the --json payload for `agent finish` (AD5-03),
// following the same additive-embed shape as agentAdoptResult.
type agentFinishResult struct {
	state.AgentRun
	SchemaVersion int      `json:"schema_version"`
	Warnings      []string `json:"warnings,omitempty"`
}

// agentFinishSchemaVersion is the contract version for agentFinishResult.
const agentFinishSchemaVersion = 1

func newAgentAdoptCommand(stdout io.Writer, opts *options) *cobra.Command {
	var engine string
	var task string
	var pid int
	var logPath string
	var adoptWorktree bool
	var projectFlag string
	var baseRefFlag string
	var allowShallow bool
	cmd := &cobra.Command{
		Use:   "adopt <worktree-path-or-id>",
		Short: "Register an agent run against a worktree a real harness (Claude Code/Cursor/Codex) already created and ran in",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			engine = strings.TrimSpace(engine)
			if engine == "" {
				return appError{code: exitUsage, err: fmt.Errorf("--engine is required")}
			}
			// --allow-shallow only reaches worktree adoption, so passing it
			// without --adopt-worktree cannot do anything. Refuse rather than
			// ignore it: a silently-inert flag reads as "shallow was allowed"
			// and is exactly how a caller concludes a refusal is a bug.
			if cmd.Flags().Changed("allow-shallow") && !adoptWorktree {
				return appError{code: exitUsage, err: fmt.Errorf("--allow-shallow only applies when --adopt-worktree registers the worktree; the worktree named here is already registered, so its base was recorded when it was adopted")}
			}
			task = strings.TrimSpace(task)
			if task == "" {
				return appError{code: exitUsage, err: fmt.Errorf("--task is required")}
			}
			if cmd.Flags().Changed("pid") && pid <= 0 {
				return appError{code: exitUsage, err: fmt.Errorf("--pid must be a positive process id")}
			}
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)

			wt, lookupErr := resolveAgentAdoptWorktree(cmd.Context(), opts, store, args[0], projectFlag)
			var warnings []string
			if lookupErr != nil {
				if !adoptWorktree {
					return appError{code: exitUsage, err: fmt.Errorf("%s is not a registered worktree (%w); pass --adopt-worktree to register it first", args[0], lookupErr)}
				}
				var outcome adoptOutcome
				var adoptErr error
				wt, _, outcome, adoptErr = adoptWorktreeAt(cmd.Context(), cmd.ErrOrStderr(), opts, store, args[0], projectFlag, baseRefFlag, allowShallow)
				if adoptErr != nil {
					return adoptErr
				}
				warnings = outcome.Warnings
			}

			runID, err := id.New("arun")
			if err != nil {
				return err
			}
			run := state.AgentRun{
				ID:          runID,
				NamespaceID: wt.NamespaceID,
				WorktreeID:  wt.ID,
				Engine:      engine,
				Task:        task,
				Status:      "running",
				BaseRef:     wt.BaseRef,
				BaseSHA:     wt.BaseSHA,
				Branch:      wt.Branch,
				LogPath:     logPath,
			}
			// --pid has NO default: a harness that shells out to invoke this
			// command has a PPID that is a transient shell about to exit, so
			// guessing os.Getppid()/os.Getpid() here would flip a healthy run
			// to "interrupted" on the very next sweep. Only record an
			// identity when the caller explicitly names the harness's own
			// pid; processStartTime failing is not fatal — the PID is still
			// recorded, just with PID-only staleness semantics (startedAt==0,
			// see processIdentityAlive).
			if cmd.Flags().Changed("pid") {
				run.RunnerPID = pid
				startedAt, _ := processStartTime(pid)
				run.RunnerStartedAt = startedAt
			}
			run, err = store.InsertAgentRun(cmd.Context(), run)
			if err != nil {
				return err
			}
			out := agentAdoptResult{AgentRun: run, SchemaVersion: agentAdoptSchemaVersion, WorktreePath: wt.Path, Warnings: warnings}
			return opts.render(stdout, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "Adopted agent run %s (engine=%s) on worktree %s\n", run.ID, run.Engine, wt.Path)
				return err
			}, out)
		},
	}
	cmd.Flags().StringVar(&engine, "engine", "", "agent engine name (free text; DevStrap is the registry, not the gatekeeper — never validated against a list)")
	cmd.Flags().StringVar(&task, "task", "", "task description")
	cmd.Flags().IntVar(&pid, "pid", 0, "the harness's own long-lived process id (never a transient shell wrapper's pid); omit if unknown (see spec/13 for the sweep/cleanup consequences of omitting it)")
	cmd.Flags().StringVar(&logPath, "log", "", "path to the harness's own run log")
	cmd.Flags().BoolVar(&adoptWorktree, "adopt-worktree", false, "register the worktree first (equivalent to 'worktree adopt') if it is not already known to DevStrap")
	cmd.Flags().StringVar(&projectFlag, "project", "", "namespace path of the project this worktree belongs to (required when it cannot be inferred uniquely; also passed through when --adopt-worktree registers a new row)")
	cmd.Flags().StringVar(&baseRefFlag, "base-ref", "", "explicit base ref passed through to worktree adoption when --adopt-worktree registers a new row")
	cmd.Flags().BoolVar(&allowShallow, "allow-shallow", false, "with --adopt-worktree, adopt even though the repository is a shallow clone (recorded base_sha may be inaccurate); requires --adopt-worktree")
	return cmd
}

// resolveAgentAdoptWorktree maps the `agent adopt` argument to an already
// registered worktree row: a worktrees.id is looked up directly; otherwise
// the argument is treated as a filesystem path and normalized exactly the way
// `worktree adopt`/adoptWorktreeAt normalize it (filepath.Abs + EvalSymlinks)
// before the owning project is inferred (via --project or the worktree's
// main checkout, the same projectForWorktreeAdopt helper adoptWorktreeAt
// uses) and Store.WorktreeByPath is queried. Parity matters here because the
// active-worktree unique index (idx_worktrees_active_path) is keyed on
// (namespace_id, path) — a differently-normalized path would miss a real row.
func resolveAgentAdoptWorktree(ctx context.Context, opts *options, store *state.Store, arg, projectFlag string) (state.Worktree, error) {
	if id.Valid("wt", arg) {
		return store.WorktreeByID(ctx, arg)
	}
	absPath, err := filepath.Abs(arg)
	if err != nil {
		return state.Worktree{}, fmt.Errorf("resolve path %q: %w", arg, err)
	}
	resolvedPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return state.Worktree{}, fmt.Errorf("%s does not exist or is not accessible: %w", arg, err)
	}
	r := gitRunner(opts)
	identity, err := r.WorktreeIdentity(ctx, resolvedPath)
	if err != nil {
		return state.Worktree{}, fmt.Errorf("%s is not a git worktree: %w", resolvedPath, err)
	}
	project, err := projectForWorktreeAdopt(ctx, opts, store, identity.MainCheckout, projectFlag)
	if err != nil {
		return state.Worktree{}, err
	}
	return store.WorktreeByPath(ctx, project.ID, resolvedPath)
}

func newAgentFinishCommand(stdout io.Writer, opts *options) *cobra.Command {
	var status string
	var testSummary string
	cmd := &cobra.Command{
		Use:   "finish <run-id>",
		Short: "Report the terminal status of an externally-run agent (Claude Code/Cursor/Codex, etc.)",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			status = strings.ToLower(strings.TrimSpace(status))
			switch status {
			case "complete", "failed":
			default:
				return appError{code: exitUsage, err: fmt.Errorf("unsupported --status %q (want complete or failed)", status)}
			}
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			run, err := store.AgentRunByID(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			switch run.Status {
			case "complete", "failed":
				return appError{code: exitConflict, err: fmt.Errorf("agent run %s is already %s; finish is not idempotent", run.ID, run.Status)}
			case "interrupted":
				if status == "complete" {
					// The sweep inferred "the recorder died" from a dead PID,
					// which is weaker evidence than the harness's own explicit
					// report — the harness wins, but the disagreement is
					// surfaced rather than silently overwritten.
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: agent run %s was reconciled to interrupted (its recorded process was found dead); recording the harness-reported complete status anyway\n", run.ID)
				}
			case "running":
				// Both complete and failed are ordinary, silent transitions.
			default:
				return appError{code: exitConflict, err: fmt.Errorf("agent run %s has unexpected status %q", run.ID, run.Status)}
			}

			testSummary = strings.TrimSpace(testSummary)
			if testSummary != "" {
				// Recompute the diff summary the same way `agent run` does at
				// completion time — an adopted run never had one set at
				// insert time, so this is the only chance to record it.
				diffSummary := run.DiffSummary
				if run.WorktreeID != "" {
					if wt, wtErr := store.WorktreeByID(cmd.Context(), run.WorktreeID); wtErr == nil {
						diffSummary = agentDiffSummary(cmd.Context(), wt.Path, wt.BaseSHA)
					}
				}
				if err := store.UpdateAgentRunResult(cmd.Context(), run.ID, status, diffSummary, testSummary); err != nil {
					return err
				}
				run.DiffSummary = diffSummary
				run.TestSummary = testSummary
			} else if err := store.UpdateAgentRunStatus(cmd.Context(), run.ID, status); err != nil {
				return err
			}
			run.Status = status
			out := agentFinishResult{AgentRun: run, SchemaVersion: agentFinishSchemaVersion}
			return opts.render(stdout, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "Agent run %s finished: %s\n", run.ID, run.Status)
				return err
			}, out)
		},
	}
	cmd.Flags().StringVar(&status, "status", "complete", "terminal status to record: complete or failed")
	cmd.Flags().StringVar(&testSummary, "test-summary", "", "free-text test/verification summary to attach to the run")
	return cmd
}
