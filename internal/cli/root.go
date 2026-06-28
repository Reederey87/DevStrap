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

func Execute(ctx context.Context) error {
	root := NewRootCommand(os.Stdout, os.Stderr)
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
	cmd.PersistentFlags().StringVar(&opts.cfgFile, "config", "", "config file path")
	cmd.PersistentFlags().BoolVar(&opts.json, "json", false, "print machine-readable JSON")
	cmd.PersistentFlags().StringVar(&opts.home, "home", "", "DevStrap state directory")
	cmd.PersistentFlags().StringVar(&opts.root, "root", "", "managed code root")
	cmd.PersistentFlags().BoolVar(&opts.quiet, "quiet", false, "only print errors")
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
	cmd.AddCommand(newHydrateCommand(stdout, opts))
	cmd.AddCommand(newOpenCommand(stdout, opts))
	cmd.AddCommand(newWorktreeCommand(stdout, opts))
	cmd.AddCommand(newSyncCommand(stdout, opts))
	cmd.AddCommand(newEnvCommand(stdout, opts))
	cmd.AddCommand(newRunCommand(stdout, opts))
	cmd.AddCommand(newAgentCommand(stdout, opts))
	cmd.AddCommand(newDevicesCommand(stdout, opts))
	cmd.AddCommand(newConflictsCommand(stdout, opts))

	return cmd
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
	return state.Open(ctx, paths.StateDB())
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
	var app appError
	if errors.As(err, &app) {
		return app.code
	}
	if errors.Is(err, dsgit.ErrAuth) {
		return exitAuth
	}
	if errors.Is(err, dsgit.ErrNetwork) {
		return exitNetwork
	}
	if errors.Is(err, dsgit.ErrBranchNotFound) || errors.Is(err, dsgit.ErrRemoteMissing) {
		return exitGit
	}
	return exitGeneric
}
