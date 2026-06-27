package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Reederey87/DevStrap/internal/childenv"
	dsgit "github.com/Reederey87/DevStrap/internal/git"
	"github.com/Reederey87/DevStrap/internal/id"
	"github.com/Reederey87/DevStrap/internal/redact"
	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/cobra"
)

func newAgentCommand(stdout io.Writer, opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Run agents in isolated fresh worktrees",
	}
	cmd.AddCommand(newAgentRunCommand(stdout, opts))
	cmd.AddCommand(newAgentListCommand(stdout, opts))
	cmd.AddCommand(newAgentShowCommand(stdout, opts))
	cmd.AddCommand(newAgentPRCommand(stdout, opts))
	return cmd
}

func newAgentRunCommand(stdout io.Writer, opts *options) *cobra.Command {
	var engine string
	var taskName string
	var commandFlag string
	var policy string
	cmd := &cobra.Command{
		Use:   "run <path> [-- command [args...]]",
		Short: "Run a generic agent command in a fresh worktree",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if taskName == "" {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("--task is required")}
			}
			engine = strings.ToLower(strings.TrimSpace(engine))
			if engine != "generic" {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("unsupported agent engine %q; only generic is implemented", engine)}
			}
			agentCommand := agentCommandArgs(args[1:], commandFlag)
			if len(agentCommand) == 0 {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("agent command is required after -- or via --command")}
			}
			policy = strings.ToLower(strings.TrimSpace(policy))
			if err := enforceAgentCommandPolicy(policy, agentCommand); err != nil {
				return err
			}
			store, err := opts.openState()
			if err != nil {
				return err
			}
			defer closeStore(store)
			project, err := store.ProjectByPath(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			wt, err := createFreshWorktree(cmd.Context(), stdout, opts, store, project, taskName, "agent")
			if err != nil {
				return err
			}
			if err := enforceAgentFilePolicy(policy, agentCommand, wt.Path); err != nil {
				return err
			}
			runID, err := id.New("arun")
			if err != nil {
				return err
			}
			logPath := filepath.Join(opts.paths().Home, "logs", "agent-runs", runID+".log")
			run, err := store.InsertAgentRun(cmd.Context(), state.AgentRun{
				ID:          runID,
				NamespaceID: project.ID,
				WorktreeID:  wt.ID,
				Engine:      engine,
				Task:        taskName,
				PolicyID:    policy,
				Status:      "running",
				BaseRef:     wt.BaseRef,
				BaseSHA:     wt.BaseSHA,
				Branch:      wt.Branch,
				LogPath:     logPath,
			})
			if err != nil {
				return err
			}
			commandErr := runAgentProcess(cmd.Context(), wt, run, agentCommand, stdout)
			diffSummary := agentDiffSummary(cmd.Context(), wt.Path)
			status := "complete"
			testSummary := "command exited 0"
			if commandErr != nil {
				status = "failed"
				testSummary = commandErr.Error()
			}
			if err := store.UpdateAgentRunResult(cmd.Context(), run.ID, status, diffSummary, testSummary); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(stdout, "\nAgent run %s %s\nworktree: %s\nlog: %s\ndiff:\n%s\n", run.ID, status, wt.Path, logPath, emptySummary(diffSummary))
			if commandErr != nil {
				return fmt.Errorf("agent run %s failed: %w", run.ID, commandErr)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&engine, "engine", "generic", "agent engine")
	cmd.Flags().StringVar(&taskName, "task", "", "task description")
	cmd.Flags().StringVar(&commandFlag, "command", "", "generic command to run, split on whitespace; args after -- are preferred")
	cmd.Flags().StringVar(&policy, "policy", "guarded", "agent command policy: readonly, cautious, guarded, or yolo-local")
	return cmd
}

func newAgentListCommand(stdout io.Writer, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List agent runs",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState()
			if err != nil {
				return err
			}
			defer closeStore(store)
			runs, err := store.ListAgentRuns(cmd.Context())
			if err != nil {
				return err
			}
			if opts.v.GetBool("json") {
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(runs)
			}
			for _, run := range runs {
				_, _ = fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\t%s\n", run.ID, run.Status, run.Engine, run.Branch, run.Task)
			}
			return nil
		},
	}
}

func newAgentShowCommand(stdout io.Writer, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show an agent run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState()
			if err != nil {
				return err
			}
			defer closeStore(store)
			run, err := store.AgentRunByID(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if opts.v.GetBool("json") {
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(run)
			}
			_, err = fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\nlog: %s\ndiff:\n%s\n", run.ID, run.Status, run.Engine, run.Task, run.LogPath, emptySummary(run.DiffSummary))
			return err
		},
	}
}

func newAgentPRCommand(stdout io.Writer, opts *options) *cobra.Command {
	var allowStaleBase bool
	var dryRun bool
	var title string
	var body string
	cmd := &cobra.Command{
		Use:   "pr <agent-run-id>",
		Short: "Create a GitHub PR after the stale-base gate",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState()
			if err != nil {
				return err
			}
			defer closeStore(store)
			run, err := store.AgentRunByID(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if run.WorktreeID == "" {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("agent run %s has no worktree", run.ID)}
			}
			wt, err := store.WorktreeByID(cmd.Context(), run.WorktreeID)
			if err != nil {
				return err
			}
			drift, err := finalizationBaseDrift(cmd.Context(), wt)
			if err != nil {
				return err
			}
			if !drift.Fresh && !allowStaleBase {
				return appError{code: exitConflict, err: fmt.Errorf("base %s moved %d commits; rebase or pass --allow-stale-base", wt.BaseRef, drift.Behind)}
			}
			baseBranch := strings.TrimPrefix(wt.BaseRef, "origin/")
			if title == "" {
				title = run.Task
			}
			if body == "" {
				body = agentPRBody(run)
			}
			if dryRun {
				_, err = fmt.Fprintf(stdout, "Would create PR for %s with base %s and head %s\n", run.ID, baseBranch, wt.Branch)
				return err
			}
			if err := pushAgentBranch(cmd.Context(), wt.Path, wt.Branch); err != nil {
				return err
			}
			url, err := createAgentPR(cmd.Context(), wt.Path, baseBranch, wt.Branch, title, body)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(stdout, "Created PR for %s: %s\n", run.ID, url)
			return err
		},
	}
	cmd.Flags().BoolVar(&allowStaleBase, "allow-stale-base", false, "allow PR creation even when the recorded base moved")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show the PR command without pushing or creating it")
	cmd.Flags().StringVar(&title, "title", "", "PR title")
	cmd.Flags().StringVar(&body, "body", "", "PR body")
	return cmd
}

func agentCommandArgs(args []string, commandFlag string) []string {
	if len(args) > 0 {
		return args
	}
	return strings.Fields(commandFlag)
}

func enforceAgentCommandPolicy(policy string, args []string) error {
	if policy == "" {
		policy = "guarded"
	}
	switch policy {
	case "readonly", "cautious", "guarded":
	case "yolo-local":
		return nil
	default:
		return appError{code: exitInvalidConfig, err: fmt.Errorf("unsupported agent policy %q", policy)}
	}
	joined := strings.ToLower(strings.Join(args, " "))
	deny := []string{
		"rm -rf /",
		"cat .env",
		"cat ~/.snowflake/config.toml",
		"chmod -r 777",
		"chmod -R 777",
		"curl | sh",
		"curl -fsSL",
		"curl -sSL",
	}
	for _, pattern := range deny {
		if strings.Contains(joined, strings.ToLower(pattern)) {
			return appError{code: exitPolicy, err: fmt.Errorf("agent policy %s denied command pattern %q; pass --policy yolo-local for explicit personal override", policy, pattern)}
		}
	}
	if policy == "readonly" {
		for _, token := range []string{" sh -c ", " tee ", ">", ">>", "git add", "git commit", "git push", "npm install"} {
			if strings.Contains(" "+joined+" ", token) {
				return appError{code: exitPolicy, err: fmt.Errorf("agent policy readonly denied mutating command; use cautious, guarded, or yolo-local")}
			}
		}
	}
	return nil
}

func enforceAgentFilePolicy(policy string, args []string, worktreePath string) error {
	if policy == "yolo-local" || worktreePath == "" {
		return nil
	}
	root, err := filepath.Abs(worktreePath)
	if err != nil {
		return fmt.Errorf("resolve agent worktree path: %w", err)
	}
	for _, token := range agentPathTokens(args) {
		if token == "" {
			continue
		}
		resolved, pathLike := resolveAgentPathToken(root, token)
		if agentTokenLooksSensitive(token) || (pathLike && agentPathLooksSensitive(root, resolved)) {
			return appError{code: exitPolicy, err: fmt.Errorf("agent file policy %s denied sensitive path %q; pass --policy yolo-local for explicit personal override", policy, token)}
		}
		if !pathLike {
			continue
		}
		if !pathWithin(root, resolved) {
			return appError{code: exitPolicy, err: fmt.Errorf("agent file policy %s denied path outside worktree %q; pass --policy yolo-local for explicit personal override", policy, token)}
		}
	}
	return nil
}

func agentPathTokens(args []string) []string {
	var tokens []string
	for _, arg := range args {
		tokens = append(tokens, arg)
		if strings.ContainsAny(arg, " \t\n") {
			tokens = append(tokens, strings.Fields(arg)...)
		}
		if _, value, ok := strings.Cut(arg, "="); ok {
			tokens = append(tokens, value)
		}
	}
	return tokens
}

func resolveAgentPathToken(root, token string) (string, bool) {
	token = strings.TrimSpace(token)
	token = strings.Trim(token, `"'`)
	token = strings.TrimRight(token, ",;")
	if token == "" || token == "-" || strings.HasPrefix(token, "http://") || strings.HasPrefix(token, "https://") || strings.HasPrefix(token, "git@") {
		return "", false
	}
	if strings.HasPrefix(token, "--") && !strings.Contains(token, "=") {
		return "", false
	}
	if strings.HasPrefix(token, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		return filepath.Clean(filepath.Join(home, strings.TrimPrefix(token, "~/"))), true
	}
	if filepath.IsAbs(token) {
		return filepath.Clean(token), true
	}
	if strings.HasPrefix(token, ".") || strings.Contains(token, "/") || agentTokenLooksSensitive(token) {
		return filepath.Clean(filepath.Join(root, token)), true
	}
	return "", false
}

func agentTokenLooksSensitive(token string) bool {
	token = strings.ToLower(strings.Trim(strings.TrimSpace(token), `"'`))
	base := strings.ToLower(filepath.Base(token))
	if base == ".env" || strings.HasPrefix(base, ".env.") {
		return true
	}
	switch base {
	case ".netrc", ".npmrc", ".pypirc", "id_rsa", "id_ed25519":
		return true
	default:
		return false
	}
}

func agentPathLooksSensitive(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err == nil && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".." {
		parts := strings.Split(filepath.ToSlash(rel), "/")
		for _, part := range parts {
			part = strings.ToLower(part)
			if part == ".env" || strings.HasPrefix(part, ".env.") {
				return true
			}
		}
	}
	lower := strings.ToLower(filepath.ToSlash(path))
	denyParts := []string{"/.ssh", "/.aws", "/.snowflake", "/.config/gh", "/.gnupg"}
	for _, part := range denyParts {
		if strings.Contains(lower, part+"/") || strings.HasSuffix(lower, part) {
			return true
		}
	}
	return agentTokenLooksSensitive(path)
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func runAgentProcess(ctx context.Context, wt state.Worktree, run state.AgentRun, args []string, stdout io.Writer) error {
	if err := os.MkdirAll(filepath.Dir(run.LogPath), 0o700); err != nil {
		return fmt.Errorf("create agent log dir: %w", err)
	}
	logFile, err := os.OpenFile(run.LogPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // Agent log path is generated under DevStrap home from the agent run id.
	if err != nil {
		return fmt.Errorf("create agent log: %w", err)
	}
	defer func() { _ = logFile.Close() }()
	env, err := childenv.FromOS(childenv.BasicAllowlist(), map[string]string{
		"DEVSTRAP_AGENT_RUN_ID": run.ID,
		"DEVSTRAP_WORKTREE_ID":  wt.ID,
	})
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, args[0], args[1:]...) //nolint:gosec // agent generic command is explicit user-selected argv, run in an isolated worktree with sanitized env.
	command.Dir = wt.Path
	command.Env = env
	// Scrub secrets (token shapes a tool may echo) from both the live output
	// and the persisted 0600 log so credentials never land on disk in
	// cleartext. A single scrubbing Writer per sink serializes concurrent
	// stdout/stderr copier goroutines.
	logScrub := redact.NewWriter(logFile, nil)
	outScrub := redact.NewWriter(stdout, nil)
	command.Stdout = io.MultiWriter(outScrub, logScrub)
	command.Stderr = io.MultiWriter(outScrub, logScrub)
	command.Stdin = os.Stdin
	runErr := command.Run()
	flushErr := errors.Join(outScrub.Close(), logScrub.Close())
	if runErr != nil {
		return runErr
	}
	return flushErr
}

func agentDiffSummary(ctx context.Context, worktreePath string) string {
	r := dsgit.NewRunner()
	status, statusErr := r.Run(ctx, worktreePath, "status", "--short")
	out, err := r.Run(ctx, worktreePath, "diff", "--stat")
	if err != nil {
		if statusErr != nil {
			return "diff unavailable: " + err.Error()
		}
		return strings.TrimSpace(status)
	}
	parts := []string{strings.TrimSpace(status), strings.TrimSpace(out)}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func emptySummary(value string) string {
	if strings.TrimSpace(value) == "" {
		return "(no changes)"
	}
	return value
}

func agentPRBody(run state.AgentRun) string {
	var body strings.Builder
	body.WriteString("DevStrap agent run: ")
	body.WriteString(run.ID)
	body.WriteString("\n\nTask:\n")
	body.WriteString(run.Task)
	if run.DiffSummary != "" {
		body.WriteString("\n\nDiff summary:\n")
		body.WriteString(run.DiffSummary)
	}
	return body.String()
}

func pushAgentBranch(ctx context.Context, dir, branch string) error {
	if _, err := dsgit.NewRunner().Run(ctx, dir, "push", "-u", "origin", branch); err != nil {
		return appError{code: exitGit, err: err}
	}
	return nil
}

func createAgentPR(ctx context.Context, dir, baseBranch, headBranch, title, body string) (string, error) {
	env, err := childenv.FromOS(append(childenv.BasicAllowlist(), "GH_*", "GITHUB_TOKEN"), nil)
	if err != nil {
		return "", err
	}
	command := exec.CommandContext(ctx, "gh", "pr", "create", "--base", baseBranch, "--head", headBranch, "--title", title, "--body", body) //nolint:gosec // fixed GitHub CLI command with explicit argv, sanitized env, and user-authored title/body data.
	command.Dir = dir
	command.Env = env
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", appError{code: exitGit, err: fmt.Errorf("gh pr create failed: %s", msg)}
	}
	return strings.TrimSpace(stdout.String()), nil
}
