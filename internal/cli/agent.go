package cli

import (
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
			store, err := opts.openState(cmd.Context())
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
				// M2: clean up the just-created worktree so a policy denial
				// does not leak an orphan git worktree + DB row. Shares the
				// P6-GIT-05 helper: detached bounded context + surfaced
				// warnings instead of swallowed errors.
				repoPath := project.LocalPath
				if repoPath == "" {
					repoPath = filepath.Join(opts.paths().Root, filepath.FromSlash(project.Path))
				}
				removeOrphanWorktree(cmd.Context(), cmd.ErrOrStderr(), gitRunner(opts), repoPath, wt.Path, wt.Branch)
				if markErr := store.MarkWorktreeRemoved(cmd.Context(), wt.ID); markErr != nil {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to mark worktree %s removed: %v\n", wt.ID, markErr)
				}
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
			diffSummary := agentDiffSummary(cmd.Context(), wt.Path, wt.BaseSHA)
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
				// CLI-03: propagate the child's real exit code.
				var ee *exec.ExitError
				if errors.As(commandErr, &ee) {
					return appError{code: childExitBase + ee.ExitCode(), err: fmt.Errorf("agent run %s failed: command exited %d", run.ID, ee.ExitCode())}
				}
				return fmt.Errorf("agent run %s failed: %w", run.ID, commandErr)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&engine, "engine", "generic", "agent engine")
	cmd.Flags().StringVar(&taskName, "task", "", "task description")
	cmd.Flags().StringVar(&commandFlag, "command", "", "generic command to run, split on whitespace; args after -- are preferred")
	cmd.Flags().StringVar(&policy, "policy", "guarded", "agent command policy: readonly, cautious, guarded, or yolo-local (advisory only — not a security boundary until OS sandboxing lands; AGEN-01)")
	return cmd
}

func newAgentListCommand(stdout io.Writer, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List agent runs",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState(cmd.Context())
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
			store, err := opts.openState(cmd.Context())
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
	var forgeFlag string
	cmd := &cobra.Command{
		Use:   "pr <agent-run-id>",
		Short: "Create a PR/MR after the stale-base gate (forge-agnostic: gh/glab/tea)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState(cmd.Context())
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
			// GIT-05: resolve the per-project forge override so a self-hosted
			// GitLab/Gitea remote routes to glab/tea instead of degrading.
			var projectForge string
			if p, err := store.ProjectByID(cmd.Context(), run.NamespaceID); err == nil {
				projectForge = p.ForgeKind
			}
			drift, err := finalizationBaseDrift(cmd.Context(), opts, wt)
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
			if err := pushAgentBranch(cmd.Context(), opts, wt.Path, wt.Branch); err != nil {
				return err
			}
			url, err := createAgentPR(cmd.Context(), opts, wt.Path, baseBranch, wt.Branch, title, body, forgeFlag, projectForge, forgeHostMap(opts.v))
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
	cmd.Flags().StringVar(&forgeFlag, "forge", "", "forge kind override (github|gitlab|gitea|bitbucket|azure) for self-hosted instances (GIT-05)")
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
	case "readonly", "cautious", "guarded", "ephemeral-ci":
	case "yolo-local":
		return nil
	default:
		return appError{code: exitInvalidConfig, err: fmt.Errorf("unsupported agent policy %q", policy)}
	}
	// AGEN-01: block known interpreters/shells/downloaders under non-yolo
	// policies. argv-substring matching is bypassable by any interpreter, so
	// refusing bare interpreters reduces the most obvious exfil vector. This
	// is advisory, NOT a security boundary — only an OS sandbox (AGEN-03)
	// can truly confine agent code.
	interpreters := map[string]bool{
		"sh": true, "bash": true, "zsh": true, "env": true,
		"python": true, "python3": true, "python2": true,
		"node": true, "perl": true, "ruby": true,
		"wget": true, "curl": true,
	}
	if len(args) > 0 && interpreters[strings.ToLower(filepath.Base(args[0]))] {
		return appError{code: exitPolicy, err: fmt.Errorf("agent policy %s denied interpreter/downloader %q; pass --policy yolo-local for explicit personal override", policy, args[0])}
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
		// AGEN-04: use argv-aware redirection detection instead of substring
		// matching so `grep 'a->b'` is not falsely denied.
		for _, arg := range args {
			if arg == ">" || arg == ">>" {
				return appError{code: exitPolicy, err: fmt.Errorf("agent policy readonly denied redirection; use cautious, guarded, or yolo-local")}
			}
		}
		for _, token := range []string{" sh -c ", " tee ", "git add", "git commit", "git push", "npm install"} {
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
	// AGEN-05: match the scan detector's sensitive-file set so the agent
	// deny list and the scanner cannot drift.
	switch base {
	case ".netrc", ".npmrc", ".pypirc", "id_rsa", "id_ed25519",
		"credentials.json", "service-account.json":
		return true
	}
	if strings.Contains(base, "service-account") {
		return true
	}
	if strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key") {
		return true
	}
	return false
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
	// AGEN-05: expanded deny set to match the spec and scan detector.
	denyParts := []string{"/.ssh", "/.aws", "/.snowflake", "/.config/gh", "/.gnupg", "/.kube", "/.docker"}
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
	env, err := childenv.FromOS(childenv.AgentAllowlist(), map[string]string{
		"DEVSTRAP_AGENT_RUN_ID": run.ID,
		"DEVSTRAP_WORKTREE_ID":  wt.ID,
		"HOME":                  wt.Path, // SECU-02: repoint HOME to worktree so agent tooling cannot reach user dotfiles.
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

func agentDiffSummary(ctx context.Context, worktreePath, baseSHA string) string {
	// P6-GIT-01: this helper only reads local status/diff state, so it does
	// not need the materialization clone_timeout plumbing.
	r := dsgit.NewRunner()
	if _, err := r.Run(ctx, worktreePath, "rev-parse", "--verify", "HEAD"); err != nil {
		return agentWorkingTreeDiffSummary(ctx, r, worktreePath)
	}

	status, statusErr := r.Run(ctx, worktreePath, "status", "--short")
	committed := "diff unavailable: missing base SHA"
	if strings.TrimSpace(baseSHA) != "" {
		out, err := r.Run(ctx, worktreePath, "diff", "--stat", strings.TrimSpace(baseSHA)+"..HEAD")
		if err != nil {
			committed = "diff unavailable: " + err.Error()
		} else {
			committed = emptySummary(strings.TrimSpace(out))
		}
	}

	uncommitted := emptySummary(strings.TrimSpace(status))
	if statusErr != nil {
		uncommitted = "status unavailable: " + statusErr.Error()
	}

	parts := []string{
		"Committed since base:",
		committed,
		"",
		"Uncommitted:",
		uncommitted,
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func agentWorkingTreeDiffSummary(ctx context.Context, r dsgit.Runner, worktreePath string) string {
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

func pushAgentBranch(ctx context.Context, opts *options, dir, branch string) error {
	// P6-GIT-01: a large agent branch push is a network transfer like clone,
	// so it gets the long deadline, not the 2m default.
	if err := gitRunner(opts).PushBranch(ctx, dir, "origin", branch); err != nil {
		return appError{code: exitGit, err: err}
	}
	return nil
}

func createAgentPR(ctx context.Context, opts *options, dir, baseBranch, headBranch, title, body, forgeOverride, projectForge string, hostMap map[string]ForgeKind) (string, error) {
	// FORGE-01: detect the forge from the remote URL and route PR creation
	// accordingly (gh/glab/tea), with graceful degradation for unknown forges.
	remoteURL, err := gitRunner(opts).RemoteURL(ctx, dir)
	if err != nil {
		remoteURL = ""
	}
	// AGEN-06: scrub the PR body so token-shaped secrets never reach the forge.
	scrubbedBody := redact.Scrub(body)
	return createForgePR(ctx, dir, remoteURL, baseBranch, headBranch, title, scrubbedBody, forgeOverride, projectForge, hostMap)
}
