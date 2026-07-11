package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Reederey87/DevStrap/internal/childenv"
	dsgit "github.com/Reederey87/DevStrap/internal/git"
	"github.com/Reederey87/DevStrap/internal/id"
	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/redact"
	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/cobra"
)

// sandboxBackend resolves the platform sandbox adapter; a test seam like
// init.go's keychainBackend.
var sandboxBackend = func() platform.Sandbox { return platform.Detect().Sandbox }

// sandboxSeccompEnv is the escape hatch for the seccomp syscall denylist:
// empty or "on" installs it (the default), "off" disables it, any other value
// fails closed with the invalid-config exit class — a typo must never silently
// weaken the sandbox, mirroring DEVSTRAP_SANDBOX_BACKEND.
const sandboxSeccompEnv = "DEVSTRAP_SANDBOX_SECCOMP"

// sandboxReadConfineEnv sets the default --read-confine mode (auto|on|off).
const sandboxReadConfineEnv = "DEVSTRAP_SANDBOX_READ_CONFINE"

// agentSandboxLaunch carries the resolved sandbox decision from flag/policy
// resolution into runAgentProcess.
type agentSandboxLaunch struct {
	sandbox      platform.Sandbox
	enabled      bool
	denyNetwork  bool
	denySyscalls bool
	devstrapHome string
	mode         string
	backendName  string
	limitations  []string
	readConfine  bool
	readAllow    []string
}

// agentSandboxSpec builds the SandboxSpec for one agent run. The child env
// repoints HOME to the worktree (SECU-02), but the sensitive-read denies
// anchor on the REAL user home — the dotfiles are still on disk regardless of
// what $HOME says. It fails closed when that home cannot be resolved: an
// empty anchor would silently drop every home-anchored credential deny while
// still reporting the run as sandboxed (post-merge review, PR #107).
// `--sandbox off` is the explicit escape hatch.
func agentSandboxSpec(worktreeDir, perRunTmp, logDir string, gitDirs []string, launch agentSandboxLaunch, runID string) (platform.SandboxSpec, error) {
	userHome, err := os.UserHomeDir()
	if err != nil {
		return platform.SandboxSpec{}, fmt.Errorf("resolve user home for sandbox credential denies (use --sandbox off to run unconfined): %w", err)
	}
	return platform.SandboxSpec{
		WorktreeDir:           worktreeDir,
		TmpDir:                perRunTmp,
		LogDir:                logDir,
		UserHome:              userHome,
		DevstrapHome:          launch.devstrapHome,
		DenyNetwork:           launch.denyNetwork,
		DenySensitiveReads:    true,
		DenyDangerousSyscalls: launch.denySyscalls,
		ViolationTag:          sandboxViolationTag(runID),
		ReadConfine:           launch.readConfine,
		ReadAllowExtra:        launch.readAllow,
		GitDirs:               gitDirs,
	}, nil
}

// resolveAgentSandbox turns --sandbox mode x policy x host availability into
// a launch decision (P4-GIT-03 slice 1). Modes:
//
//	auto    — sandbox when the host adapter is available; otherwise warn once
//	          and run with today's advisory-only behavior.
//	require — refuse to run (policy exit class) when unavailable.
//	off     — never sandbox, no warning.
//
// yolo-local keeps its existing "explicit personal override" semantics: the
// sandbox is off, and combining it with --sandbox require is a config error.
// readonly/cautious additionally deny the child network access; guarded and
// ephemeral-ci sandbox the filesystem but keep the network open.
func resolveAgentSandbox(mode, policy, readConfineMode string, readAllow []string, stderr io.Writer, devstrapHome string) (agentSandboxLaunch, error) {
	launch := agentSandboxLaunch{devstrapHome: devstrapHome, readAllow: readAllow}
	launch.mode = mode
	switch mode {
	case "auto", "off", "require":
	default:
		return launch, appError{code: exitInvalidConfig, err: fmt.Errorf("unsupported --sandbox mode %q (want auto, off, or require)", mode)}
	}
	if policy == "yolo-local" {
		if mode == "require" {
			return launch, appError{code: exitInvalidConfig, err: fmt.Errorf("--sandbox require conflicts with --policy yolo-local (yolo-local runs unconfined by definition)")}
		}
		return launch, nil
	}
	if mode == "off" {
		return launch, nil
	}
	// Validate the seccomp escape hatch BEFORE host availability: a mistyped
	// DEVSTRAP_SANDBOX_SECCOMP is an explicit-config error and must fail closed
	// in every sandboxing mode, exactly like DEVSTRAP_SANDBOX_BACKEND — even
	// when `auto` would otherwise degrade to advisory because the host sandbox
	// is unavailable, so a typo can never slip through the degrade path (Codex
	// review P3). The "off" and "yolo-local" modes already returned above, so
	// the toggle is only read when a sandbox would actually run.
	denySyscalls, seccompErr := parseSeccompToggle(os.Getenv(sandboxSeccompEnv))
	if seccompErr != nil {
		return launch, seccompErr
	}
	// Validate the read-confine mode syntax up front (a typo fails closed like
	// the seccomp/backend toggles). The boolean want is derived here but only
	// applied after the backend is known and its capability is checked.
	readConfineWant, readConfineExplicit, rcErr := parseReadConfineMode(readConfineMode, policy)
	if rcErr != nil {
		return launch, rcErr
	}
	// Reject a --read-allow root that overlaps a protected credential path: read
	// confinement drops bwrap's credential masks and Landlock cannot subtract
	// from an allowed root, so such a root would silently re-expose the
	// credential on those backends. Fail closed rather than footgun (Codex
	// review). Resolve the real home here (matching agentSandboxSpec) so the
	// refusal happens before any worktree/DB row is created.
	if readConfineWant && len(readAllow) > 0 {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			// Fail closed: without the real home we cannot prove a --read-allow
			// root does not re-expose a credential, so refuse rather than skip
			// the check (CodeRabbit review — the guard must not fail open).
			return launch, fmt.Errorf("resolve user home for the --read-allow credential check (use --read-confine off to run unconfined): %w", homeErr)
		}
		if conflict := platform.FirstReadAllowCredentialConflict(home, devstrapHome, readAllow); conflict != "" {
			return launch, appError{code: exitInvalidConfig, err: fmt.Errorf("--read-allow %q overlaps a protected credential path; read confinement would re-expose it — remove that root", conflict)}
		}
	}
	sb := sandboxBackend()
	if err := sb.Available(); err != nil {
		// A mistyped DEVSTRAP_SANDBOX_BACKEND is an explicit-config error, not
		// a host capability gap: degrading it to the advisory warning would
		// let a typo silently disable the OS sandbox. Fail closed in every
		// mode (Codex review P1).
		if errors.Is(err, platform.ErrInvalidSandboxBackend) {
			return launch, appError{code: exitInvalidConfig, err: err}
		}
		if mode == "require" {
			return launch, appError{code: exitPolicy, err: fmt.Errorf("OS sandbox required but unavailable: %w", err)}
		}
		// An explicit `--read-confine on` cannot be honored without a sandbox,
		// so it must fail closed too — an explicit knob must never silently
		// no-op into an unconfined run (Codex review; mirrors --sandbox require).
		if readConfineExplicit {
			return launch, appError{code: exitPolicy, err: fmt.Errorf("--read-confine on requires an OS sandbox but none is available: %w; use --read-confine off to run unconfined", err)}
		}
		_, _ = fmt.Fprintf(stderr, "warning: OS sandbox unavailable (%v); agent policy remains advisory (AGEN-01)\n", err)
		return launch, nil
	}
	launch.sandbox = sb
	launch.enabled = true
	launch.backendName = sb.Name()
	launch.denyNetwork = policy == "readonly" || policy == "cautious"
	// Seccomp syscall denylist is unconditional hardening for every sandboxed
	// policy (validated above so a typo fails closed before this point); the
	// escape hatch only turns it off, never silently weakens the sandbox.
	launch.denySyscalls = denySyscalls
	if !denySyscalls {
		_, _ = fmt.Fprintf(stderr, "notice: OS sandbox syscall denylist disabled via %s=off; the kernel filter is not installed\n", sandboxSeccompEnv)
	}
	// A degraded backend (the Linux landlock fallback) is still a kernel
	// write-confinement boundary, so it satisfies `require` — except when the
	// policy's network deny cannot be enforced at all: running a "no network"
	// policy with the network open would break the policy's promise. A
	// TCP-only deny satisfies `require` but must never read as netns-grade
	// isolation, so it warns (adversarial review P2).
	if caps, ok := sb.(platform.SandboxCapabilities); ok {
		if launch.denyNetwork {
			switch caps.NetworkDenyEnforcement() {
			case platform.NetworkDenyNone:
				if mode == "require" {
					return launch, appError{code: exitPolicy, err: fmt.Errorf("policy %s requires a network deny but OS sandbox %s cannot enforce it; use --policy guarded, enable bubblewrap, or --sandbox off", policy, sb.Name())}
				}
				_, _ = fmt.Fprintf(stderr, "warning: OS sandbox %s cannot enforce the %s network deny; the child network stays open\n", sb.Name(), policy)
			case platform.NetworkDenyPartialTCP:
				_, _ = fmt.Fprintf(stderr, "warning: OS sandbox %s enforces the %s network deny for TCP bind/connect only; UDP, QUIC, and unix-domain sockets stay open\n", sb.Name(), policy)
			case platform.NetworkDenyTotal:
			}
		}
		if lims := caps.Limitations(); len(lims) > 0 {
			launch.limitations = lims
			_, _ = fmt.Fprintf(stderr, "notice: OS sandbox %s active with reduced guarantees: %s\n", sb.Name(), strings.Join(lims, "; "))
		}
	}
	// Read confinement (opt-in): honor it only when the selected backend can
	// kernel-enforce it. An explicit `--read-confine on` (or `require`) refuses
	// to launch if the backend cannot — an explicit knob must never silently
	// no-op; an auto-derived request (readonly policy) degrades to a warning.
	if readConfineWant {
		rc, ok := sb.(platform.SandboxReadConfinement)
		if ok && rc.ReadConfineEnforcement() == platform.ReadConfineEnforced {
			launch.readConfine = true
		} else if mode == "require" || readConfineExplicit {
			return launch, appError{code: exitPolicy, err: fmt.Errorf("read confinement requested but OS sandbox %s cannot enforce it; use --read-confine off or --sandbox off", sb.Name())}
		} else {
			_, _ = fmt.Fprintf(stderr, "warning: OS sandbox %s cannot enforce read confinement; the child's reads stay unconfined\n", sb.Name())
		}
	}
	return launch, nil
}

// parseReadConfineMode validates the --read-confine value and derives whether
// read confinement is wanted. "auto" (the default) enables it only for the
// readonly policy — the one profile already meant to be strictly read-scoped;
// cautious/guarded stay unconfined until telemetry proves the allow-list
// survives real toolchains. "on"/"off" force it; anything else fails closed.
// The returned explicit flag is true only for "on", so an explicit request is
// held to `require`-grade enforcement.
func parseReadConfineMode(mode, policy string) (want, explicit bool, err error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "auto":
		return policy == "readonly", false, nil
	case "on":
		return true, true, nil
	case "off":
		return false, false, nil
	default:
		return false, false, appError{code: exitInvalidConfig, err: fmt.Errorf("invalid %s=%q (want auto, on, or off)", sandboxReadConfineEnv, mode)}
	}
}

func sandboxViolationTag(runID string) string {
	return "devstrap-sb-" + runID
}

func marshalLimitations(limitations []string) string {
	if len(limitations) == 0 {
		return ""
	}
	b, err := json.Marshal(limitations)
	if err != nil {
		return ""
	}
	return string(b)
}

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
	var sandboxMode string
	var readConfineMode string
	var readAllow []string
	cmd := &cobra.Command{
		Use:   "run <path> [-- command [args...]]",
		Short: "Run a generic agent command in a fresh worktree",
		Args:  usageArgs(cobra.MinimumNArgs(1)),
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
			// Resolve the sandbox decision BEFORE any state is created so
			// `--sandbox require` on an unsupported host fails cheaply with
			// no orphan worktree/DB row to clean up.
			sandboxMode = strings.ToLower(strings.TrimSpace(sandboxMode))
			sandboxLaunch, err := resolveAgentSandbox(sandboxMode, policy, readConfineMode, readAllow, cmd.ErrOrStderr(), opts.paths().Home)
			if err != nil {
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
			// P7-GIT-01: hold the project repo lock from worktree creation
			// through InsertAgentRun, so `worktree cleanup` (which checks
			// running runs under the same lock) can never observe the fresh
			// worktree without its running row.
			run, wt, err := func() (state.AgentRun, state.Worktree, error) {
				unlock, err := acquireRepoLock(opts.paths().Home, project.ID)
				if err != nil {
					return state.AgentRun{}, state.Worktree{}, err
				}
				defer unlock()
				wt, err := createFreshWorktreeLocked(cmd.Context(), stdout, opts, store, project, taskName, "agent")
				if err != nil {
					return state.AgentRun{}, state.Worktree{}, err
				}
				cleanupOrphan := func() {
					// M2: clean up the just-created worktree so a failure here
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
				}
				if err := enforceAgentFilePolicy(policy, agentCommand, wt.Path); err != nil {
					cleanupOrphan()
					return state.AgentRun{}, state.Worktree{}, err
				}
				runID, err := id.New("arun")
				if err != nil {
					cleanupOrphan()
					return state.AgentRun{}, state.Worktree{}, err
				}
				logPath := filepath.Join(opts.paths().Home, "logs", "agent-runs", runID+".log")
				run, err := store.InsertAgentRun(cmd.Context(), state.AgentRun{
					ID:                 runID,
					NamespaceID:        project.ID,
					WorktreeID:         wt.ID,
					Engine:             engine,
					Task:               taskName,
					PolicyID:           policy,
					Status:             "running",
					RunnerPID:          os.Getpid(),
					BaseRef:            wt.BaseRef,
					BaseSHA:            wt.BaseSHA,
					Branch:             wt.Branch,
					LogPath:            logPath,
					SandboxBackend:     sandboxLaunch.backendName,
					SandboxMode:        sandboxLaunch.mode,
					SandboxLimitations: marshalLimitations(sandboxLaunch.limitations),
				})
				if err != nil {
					cleanupOrphan()
					return state.AgentRun{}, state.Worktree{}, err
				}
				return run, wt, nil
			}()
			if err != nil {
				return err
			}
			runStart := time.Now()
			// Resolve the worktree's git storage dirs here (we hold *options),
			// only when the sandbox is active — otherwise the grant is unused
			// and the two rev-parse forks are wasted. Best-effort: a resolution
			// failure leaves the grant empty rather than blocking the run
			// (P7-SANDBOX-01).
			var sandboxGitDirs []string
			if sandboxLaunch.enabled {
				sandboxGitDirs, _ = gitRunner(opts).WorktreeSandboxWriteDirs(cmd.Context(), wt.Path)
				if len(sandboxGitDirs) == 0 {
					// A real fresh worktree always resolves; an empty grant here
					// means the resolver hit an unexpected git error, and the
					// agent's `git commit` would be EPERM'd inside the sandbox.
					// Warn rather than fail silently (review: P7-SANDBOX-01).
					_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "warning: could not resolve the worktree's git dirs for the sandbox; git commits inside the run may be blocked (use --sandbox off if the agent must commit)")
				}
			}
			commandErr := runAgentProcess(cmd.Context(), wt, run, agentCommand, stdout, sandboxLaunch, sandboxGitDirs)
			collectSandboxViolations(cmd.Context(), cmd.ErrOrStderr(), store, run, sandboxLaunch, runStart)
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
			_, _ = fmt.Fprintf(stdout, "\nAgent run %s %s\nworktree: %s\nlog: %s\ndiff:\n%s\n", run.ID, status, wt.Path, run.LogPath, emptySummary(diffSummary))
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
	cmd.Flags().StringVar(&policy, "policy", "guarded", "agent command policy: readonly, cautious, guarded, or yolo-local (argv/file checks are advisory; combine with the OS sandbox for real confinement)")
	cmd.Flags().StringVar(&sandboxMode, "sandbox", defaultSandboxMode(), "OS sandbox mode: auto (sandbox when the host supports it; macOS Seatbelt, Linux bubblewrap with a landlock fallback — force one via DEVSTRAP_SANDBOX_BACKEND), require (refuse to run unsandboxed), or off (env: DEVSTRAP_SANDBOX). On Linux the sandbox also installs a seccomp syscall denylist; DEVSTRAP_SANDBOX_SECCOMP=off disables it")
	cmd.Flags().StringVar(&readConfineMode, "read-confine", defaultReadConfineMode(), "restrict the child's reads to the worktree/tmp, OS toolchain roots, and $HOME build caches instead of the whole disk: auto (on for --policy readonly only), on, or off (env: DEVSTRAP_SANDBOX_READ_CONFINE). Add roots with --read-allow")
	cmd.Flags().StringArrayVar(&readAllow, "read-allow", nil, "additional absolute path to keep readable under --read-confine (repeatable)")
	return cmd
}

// parseSeccompToggle reads DEVSTRAP_SANDBOX_SECCOMP: empty/"on" enables the
// denylist, "off" disables it, anything else fails closed with the
// invalid-config exit class (same posture as a mistyped
// DEVSTRAP_SANDBOX_BACKEND).
func parseSeccompToggle(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "on":
		return true, nil
	case "off":
		return false, nil
	default:
		return false, appError{code: exitInvalidConfig, err: fmt.Errorf("invalid %s=%q (want on or off)", sandboxSeccompEnv, v)}
	}
}

// defaultSandboxMode lets DEVSTRAP_SANDBOX set the --sandbox default; the
// explicit flag still wins because cobra parses it over the default.
func defaultSandboxMode() string {
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("DEVSTRAP_SANDBOX"))); v != "" {
		return v
	}
	return "auto"
}

// defaultReadConfineMode lets DEVSTRAP_SANDBOX_READ_CONFINE set the
// --read-confine default; the explicit flag still wins. Validation happens in
// parseReadConfineMode, so a bad env value surfaces the same fail-closed error
// as a bad flag.
func defaultReadConfineMode() string {
	if v := strings.ToLower(strings.TrimSpace(os.Getenv(sandboxReadConfineEnv))); v != "" {
		return v
	}
	return "auto"
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
			if _, _, err := sweepStaleAgentRuns(cmd.Context(), store); err != nil {
				return err
			}
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
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			if _, _, err := sweepStaleAgentRuns(cmd.Context(), store); err != nil {
				return err
			}
			run, err := store.AgentRunByID(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			violations, err := store.SandboxViolationsByRun(cmd.Context(), run.ID)
			if err != nil {
				return err
			}
			if opts.v.GetBool("json") {
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(struct {
					state.AgentRun
					Violations []state.SandboxViolation `json:"violations"`
				}{AgentRun: run, Violations: violations})
			}
			if _, err := fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\nlog: %s\ndiff:\n%s\n", run.ID, run.Status, run.Engine, run.Task, run.LogPath, emptySummary(run.DiffSummary)); err != nil {
				return err
			}
			if run.SandboxBackend != "" || run.SandboxMode != "" {
				_, _ = fmt.Fprintf(stdout, "sandbox: %s mode=%s\n", emptyDash(run.SandboxBackend), run.SandboxMode)
			}
			if run.SandboxLimitations != "" {
				_, _ = fmt.Fprintf(stdout, "sandbox limitations: %s\n", run.SandboxLimitations)
			}
			if len(violations) > 0 {
				_, _ = fmt.Fprintf(stdout, "sandbox violations: %d\n", len(violations))
				for _, v := range violations {
					_, _ = fmt.Fprintf(stdout, "  %s %s\n", v.Operation, emptyDash(v.Path))
				}
			}
			return nil
		},
	}
}

func newAgentPRCommand(stdout io.Writer, opts *options) *cobra.Command {
	var allowStaleBase bool
	var dryRun bool
	var allowIncomplete bool
	var title string
	var body string
	var forgeFlag string
	cmd := &cobra.Command{
		Use:   "pr <agent-run-id>",
		Short: "Create a PR/MR after the stale-base gate (forge-agnostic: gh/glab/tea)",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			if _, _, err := sweepStaleAgentRuns(cmd.Context(), store); err != nil {
				return err
			}
			run, err := store.AgentRunByID(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if run.Status != "complete" {
				msg := fmt.Sprintf("agent run %s status is %q, not complete; rerun the agent, or pass --allow-incomplete", run.ID, run.Status)
				if !allowIncomplete {
					return appError{code: exitConflict, err: errors.New(msg)}
				}
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", msg)
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
	cmd.Flags().BoolVar(&allowIncomplete, "allow-incomplete", false, "allow PR creation when the agent run is not complete")
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
	case ".netrc", ".npmrc", ".pypirc", ".gitconfig", ".git-credentials",
		"id_rsa", "id_ed25519",
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
	denyParts := []string{"/.ssh", "/.aws", "/.snowflake", "/.config/gh", "/.config/gcloud", "/.azure", "/.gnupg", "/.kube", "/.docker", "/.gitconfig", "/.git-credentials"}
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

func runAgentProcess(ctx context.Context, wt state.Worktree, run state.AgentRun, args []string, stdout io.Writer, sandboxLaunch agentSandboxLaunch, gitDirs []string) error {
	if err := os.MkdirAll(filepath.Dir(run.LogPath), 0o700); err != nil {
		return fmt.Errorf("create agent log dir: %w", err)
	}
	logFile, err := os.OpenFile(run.LogPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // Agent log path is generated under DevStrap home from the agent run id.
	if err != nil {
		return fmt.Errorf("create agent log: %w", err)
	}
	defer func() { _ = logFile.Close() }()
	envOverrides := map[string]string{
		"DEVSTRAP_AGENT_RUN_ID": run.ID,
		"DEVSTRAP_WORKTREE_ID":  wt.ID,
		"HOME":                  wt.Path, // SECU-02: repoint HOME to worktree so agent tooling cannot reach user dotfiles.
	}
	var extraFiles []*os.File
	if sandboxLaunch.enabled {
		// The write allow-list must not include the machine-wide shared
		// $TMPDIR (review P1): the child gets a PER-RUN scratch dir instead,
		// created here and torn down after the run, and its TMPDIR env is
		// repointed to match — so the kernel grant is scoped to this run,
		// never to other processes' temp files.
		perRunTmp := filepath.Join(os.TempDir(), "devstrap-agent-"+run.ID)
		if err := os.MkdirAll(perRunTmp, 0o700); err != nil {
			return fmt.Errorf("create agent tmp dir: %w", err)
		}
		defer func() { _ = os.RemoveAll(perRunTmp) }()
		envOverrides["TMPDIR"] = perRunTmp
		// gitDirs (resolved by the caller, which holds *options) grants the
		// linked worktree's git storage dirs so the agent's `git add`/`commit`
		// are not EPERM'd by the sandbox (P7-SANDBOX-01).
		spec, err := agentSandboxSpec(wt.Path, perRunTmp, filepath.Dir(run.LogPath), gitDirs, sandboxLaunch, run.ID)
		if err != nil {
			return err
		}
		sc, err := sandboxLaunch.sandbox.Command(ctx, spec, args)
		if err != nil {
			return fmt.Errorf("prepare OS sandbox: %w", err)
		}
		// Cleanup closes any inherited seccomp fd and removes generated
		// profiles; it runs after command.Run returns (defer ordering).
		defer sc.Cleanup()
		args = sc.Argv
		extraFiles = sc.ExtraFiles
	}
	env, err := childenv.FromOS(childenv.AgentAllowlist(), envOverrides)
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, args[0], args[1:]...) //nolint:gosec // agent generic command is explicit user-selected argv, run in an isolated worktree with sanitized env.
	command.Dir = wt.Path
	command.Env = env
	// The sandbox launcher may reference inherited fds (bubblewrap's
	// --seccomp <fd>); entry i becomes fd 3+i in the child. Nil when unused.
	command.ExtraFiles = extraFiles
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

func collectSandboxViolations(ctx context.Context, stderr io.Writer, store *state.Store, run state.AgentRun, sandboxLaunch agentSandboxLaunch, runStart time.Time) {
	if !sandboxLaunch.enabled {
		return
	}
	reporter, ok := sandboxLaunch.sandbox.(platform.SandboxViolationReporter)
	if !ok {
		return
	}
	vs, err := reporter.CollectViolations(ctx, sandboxViolationTag(run.ID), runStart)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: sandbox violation collection failed: %v\n", err)
		return
	}
	if len(vs) == 0 {
		return
	}
	rows := make([]state.SandboxViolation, 0, len(vs))
	now := state.TimestampNow()
	for i, v := range vs {
		path := redact.Scrub(v.Path)
		rows = append(rows, state.SandboxViolation{
			RunID:      run.ID,
			ObservedAt: now,
			Backend:    sandboxLaunch.backendName,
			Operation:  v.Operation,
			Path:       path,
			Detail:     redact.Scrub(v.Detail),
			Source:     "seatbelt-log",
		})
		if i < 50 {
			slog.Warn("sandbox.violation", "run", run.ID, "backend", sandboxLaunch.backendName, "operation", v.Operation, "path", path)
		}
	}
	if err := store.InsertSandboxViolations(ctx, rows); err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: recording sandbox violations failed: %v\n", err)
	}
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

func emptyDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
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
