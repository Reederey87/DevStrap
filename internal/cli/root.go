package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Reederey87/DevStrap/internal/config"
	dsgit "github.com/Reederey87/DevStrap/internal/git"
	"github.com/Reederey87/DevStrap/internal/logging"
	"github.com/Reederey87/DevStrap/internal/redact"
	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const (
	exitGeneric           = 1
	exitInvalidConfig     = 2
	exitDaemonUnavailable = 3 // reserved for M5 daemon (ARCH2-04); not yet returned.
	exitConflict          = 4
	exitDirtyWorktree     = 5
	exitAuth              = 6
	exitGit               = 7
	exitNetwork           = 8
	exitPolicy            = 9
	exitUsage             = 10  // CLI-04: bad-flag/missing-flag/arg-count usage errors
	childExitBase         = 100 // CLI-03: child exit codes propagated as 100+N to avoid collision with reserved 1-9.
)

type appError struct {
	code int
	err  error
}

func (e appError) Error() string { return e.err.Error() }
func (e appError) Unwrap() error { return e.err }

type options struct {
	cfgFile string
	json    bool
	home    string
	root    string
	quiet   bool
	verbose int
	v       *viper.Viper
}

func (o *options) progressf(w io.Writer, format string, a ...any) {
	if o.quiet {
		return
	}
	_, _ = fmt.Fprintf(w, format, a...)
}

func Execute(ctx context.Context) error {
	root := NewRootCommand(os.Stdout, os.Stderr) //nolint:contextcheck // cobra FP: ctx flows via SetContext/cmd.Context(), which contextcheck can't trace
	root.SetContext(ctx)
	return root.Execute()
}

func NewRootCommand(stdout, stderr io.Writer) *cobra.Command {
	opts := &options{v: viper.New()}
	cmd := &cobra.Command{
		Use:           "devstrap",
		Short:         "Manage a local-first developer workspace namespace",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if err := initConfig(opts); err != nil {
				return err
			}
			logging.Configure(cmd.ErrOrStderr(), opts.v.GetBool("json"), opts.quiet, opts.verbose)
			return nil
		},
	}

	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return appError{code: exitUsage, err: err}
	})
	cmd.PersistentFlags().StringVar(&opts.cfgFile, "config", "", "config file path")
	cmd.PersistentFlags().BoolVar(&opts.json, "json", false, "print machine-readable JSON")
	cmd.PersistentFlags().StringVar(&opts.home, "home", "", "DevStrap state directory")
	cmd.PersistentFlags().StringVar(&opts.root, "root", "", "managed code root")
	cmd.PersistentFlags().BoolVar(&opts.quiet, "quiet", false, "suppress progress output (results and errors still print)")
	cmd.PersistentFlags().CountVarP(&opts.verbose, "verbose", "v", "increase log verbosity")

	_ = opts.v.BindPFlag("home", cmd.PersistentFlags().Lookup("home"))
	_ = opts.v.BindPFlag("root", cmd.PersistentFlags().Lookup("root"))
	_ = opts.v.BindPFlag("json", cmd.PersistentFlags().Lookup("json"))

	cmd.AddCommand(newVersionCommand(stdout))
	cmd.AddCommand(newInitCommand(stdout, opts))
	cmd.AddCommand(newStatusCommand(stdout, opts))
	cmd.AddCommand(newDoctorCommand(stdout, opts))
	cmd.AddCommand(newDBCommand(stdout, opts))
	cmd.AddCommand(newScanCommand(stdout, opts))
	cmd.AddCommand(newAddCommand(stdout, opts))
	cmd.AddCommand(newCloneCommand(stdout, opts))
	cmd.AddCommand(newHydrateCommand(stdout, opts))
	cmd.AddCommand(newOpenCommand(stdout, opts))
	cmd.AddCommand(newWorktreeCommand(stdout, opts))
	cmd.AddCommand(newSyncCommand(stdout, opts))
	cmd.AddCommand(newHubCommand(stdout, opts))
	cmd.AddCommand(newKeysCommand(stdout, opts))
	cmd.AddCommand(newMaterializeCommand(stdout, opts))
	cmd.AddCommand(newDraftCommand(stdout, opts))
	cmd.AddCommand(newEnvCommand(stdout, opts))
	cmd.AddCommand(newRunCommand(stdout, opts))
	cmd.AddCommand(newRunLoopCommand(stdout, opts))
	cmd.AddCommand(newServiceCommand(stdout, opts))
	cmd.AddCommand(newAgentCommand(stdout, opts))
	cmd.AddCommand(newDevicesCommand(stdout, opts))
	cmd.AddCommand(newConflictsCommand(stdout, opts))
	cmd.AddCommand(newSandboxHelperCommand())

	attachShellCompletions(cmd, opts)
	return cmd
}

// attachShellCompletions wires dynamic shell completion onto the command tree
// (P5-DX-01): namespace-path arguments complete from the local store, and enum
// flags complete from their fixed value sets.
func attachShellCompletions(root *cobra.Command, opts *options) {
	pathCompletion := completePaths(opts)
	pathCmds := map[string]bool{
		"devstrap open": true, "devstrap hydrate": true, "devstrap materialize": true,
		"devstrap worktree new": true, "devstrap env capture": true, "devstrap env hydrate": true,
		"devstrap env bind": true, "devstrap env rotate": true, "devstrap run": true,
		"devstrap draft snapshot create": true, "devstrap agent run": true,
	}
	walkCommands(root, func(c *cobra.Command) {
		if pathCmds[c.CommandPath()] {
			c.ValidArgsFunction = pathCompletion
		}
		if c.Flags().Lookup("lfs-policy") != nil {
			_ = c.RegisterFlagCompletionFunc("lfs-policy", completeEnum("auto", "never", "agent", "always"))
		}
		if c.Flags().Lookup("forge") != nil {
			_ = c.RegisterFlagCompletionFunc("forge", completeEnum("github", "gitlab", "gitea", "bitbucket", "azure"))
		}
	})
}

func walkCommands(c *cobra.Command, fn func(*cobra.Command)) {
	fn(c)
	for _, sub := range c.Commands() {
		walkCommands(sub, fn)
	}
}

// usageArgs wraps a cobra positional-args validator so a failure classifies as
// exitUsage (P6-CLI-03) instead of falling through to exitGeneric.
func usageArgs(pa cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := pa(cmd, args); err != nil {
			return appError{code: exitUsage, err: err}
		}
		return nil
	}
}

func initConfig(opts *options) error {
	opts.v.SetConfigType("yaml")
	opts.v.SetEnvPrefix("DEVSTRAP")
	opts.v.SetEnvKeyReplacer(strings.NewReplacer("-", "_", ".", "_"))
	opts.v.AutomaticEnv()

	defaults, err := config.DefaultPaths()
	if err != nil {
		return appError{code: exitInvalidConfig, err: err}
	}
	opts.v.SetDefault("home", defaults.Home)
	opts.v.SetDefault("root", defaults.Root)
	opts.v.SetDefault("materialization.clone_timeout", "30m")
	opts.v.SetDefault("sync.key_grant_grace", "72h")
	opts.v.SetDefault("keys.rotate_max_age", "2160h")
	opts.v.SetDefault(durabilityExportConfigKey, defaultDurabilityExportInterval.String())

	if opts.cfgFile != "" {
		opts.v.SetConfigFile(opts.cfgFile)
	} else {
		opts.v.SetConfigName("config")
		opts.v.AddConfigPath(opts.v.GetString("home"))
	}

	if err := opts.v.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if !errors.As(err, &notFound) {
			return appError{code: exitInvalidConfig, err: fmt.Errorf("read config: %w", err)}
		}
	}

	return nil
}

func (o *options) paths() config.Paths {
	return config.Paths{
		Home: o.v.GetString("home"),
		Root: o.v.GetString("root"),
	}
}

func (o *options) openState(ctx context.Context) (*state.Store, error) {
	paths := config.Paths{
		Home: o.v.GetString("home"),
		Root: o.v.GetString("root"),
	}
	checkJournal := func() error {
		if _, err := os.Stat(restoreJournalPath(paths.Home)); err == nil {
			return appError{code: exitConflict, err: fmt.Errorf("an interrupted 'db restore' left the state dir mid-swap; run 'devstrap db restore --recover'")}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect restore journal: %w", err)
		}
		return nil
	}
	if err := checkJournal(); err != nil {
		return nil, err
	}
	store, err := state.Open(ctx, paths.StateDB())
	if err != nil {
		return nil, err
	}
	// Re-check after opening: a restore that began between the first stat and
	// state.Open would otherwise hand back a handle onto a mid-swap database.
	// The journal is durable before the first rename, so the two checks narrow
	// this TOCTOU to the sub-rename residual documented in the threat model.
	if err := checkJournal(); err != nil {
		closeStore(store)
		return nil, err
	}
	return store, nil
}

func closeStore(store *state.Store) {
	_ = store.Close()
}

func ExitCode(err error) int {
	return ExitCodeWithWriter(err, os.Stderr)
}

func ExitCodeWithWriter(err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	// Scrub token-shaped secrets and URL credentials from the final error text
	// so a leaked value never reaches the terminal/CI logs (ENV-2/SEC-3).
	_, _ = fmt.Fprintln(stderr, redact.Scrub(err.Error()))
	// The remedy hint must print before the appError early return: an
	// auth-class git failure wrapped in appError keeps its wrapped exit code
	// but still deserves the hint (errors.As/Is both traverse the chain).
	if errors.Is(err, dsgit.ErrAuth) {
		_, _ = fmt.Fprintln(stderr, "hint: git authentication failed — check ssh key / repo access (load your key: ssh-add ~/.ssh/<key>)")
	}
	var app appError
	if errors.As(err, &app) {
		return app.code
	}
	if errors.Is(err, dsgit.ErrAuth) {
		return exitAuth
	}
	if errors.Is(err, dsgit.ErrNetwork) || errors.Is(err, dsgit.ErrTimeout) {
		return exitNetwork
	}
	if errors.Is(err, dsgit.ErrBranchNotFound) || errors.Is(err, dsgit.ErrRemoteMissing) {
		return exitGit
	}
	// P6-CLI-03: an unknown top-level subcommand is resolved inside cobra's
	// Find() before any RunE/PersistentPreRunE/Args validator runs (see
	// github.com/spf13/cobra@v1.10.2 command.go:757-778, args.go:27-38), so it
	// can never be wrapped in appError like every other usage error above.
	// Root's Args field can't be set to intercept it either: execute() checks
	// `!c.Runnable()` and returns flag.ErrHelp before ever calling
	// ValidateArgs (command.go:952-966), and root has no Run/RunE. Matching
	// cobra's own stable "unknown command %q for %q" format here is the least
	// brittle interception point available; any legitimately Args-validated
	// error with this exact text is already caught and wrapped by usageArgs
	// above and returns via the appError branch, never reaching this fallback.
	if strings.HasPrefix(err.Error(), `unknown command "`) {
		return exitUsage
	}
	return exitGeneric
}
