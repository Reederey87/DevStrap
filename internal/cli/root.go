package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/Reederey87/DevStrap/internal/config"
	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const (
	exitGeneric       = 1
	exitInvalidConfig = 2
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
}

func Execute(ctx context.Context) error {
	root := NewRootCommand(os.Stdout, os.Stderr)
	root.SetContext(ctx)
	return root.Execute()
}

func NewRootCommand(stdout, stderr io.Writer) *cobra.Command {
	opts := &options{}
	cmd := &cobra.Command{
		Use:           "devstrap",
		Short:         "Manage a local-first developer workspace namespace",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return initConfig(opts)
		},
	}

	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.PersistentFlags().StringVar(&opts.cfgFile, "config", "", "config file path")
	cmd.PersistentFlags().BoolVar(&opts.json, "json", false, "print machine-readable JSON")
	cmd.PersistentFlags().StringVar(&opts.home, "home", "", "DevStrap state directory")
	cmd.PersistentFlags().StringVar(&opts.root, "root", "", "managed code root")

	_ = viper.BindPFlag("home", cmd.PersistentFlags().Lookup("home"))
	_ = viper.BindPFlag("root", cmd.PersistentFlags().Lookup("root"))
	_ = viper.BindPFlag("json", cmd.PersistentFlags().Lookup("json"))

	cmd.AddCommand(newVersionCommand(stdout))
	cmd.AddCommand(newInitCommand(stdout))
	cmd.AddCommand(newStatusCommand(stdout))
	cmd.AddCommand(newDoctorCommand(stdout))

	return cmd
}

func initConfig(opts *options) error {
	viper.SetConfigType("yaml")
	viper.SetEnvPrefix("DEVSTRAP")
	viper.AutomaticEnv()

	defaults, err := config.DefaultPaths()
	if err != nil {
		return appError{code: exitInvalidConfig, err: err}
	}
	viper.SetDefault("home", defaults.Home)
	viper.SetDefault("root", defaults.Root)

	if opts.cfgFile != "" {
		viper.SetConfigFile(opts.cfgFile)
	} else {
		viper.SetConfigName("config")
		viper.AddConfigPath(defaults.Home)
	}

	if err := viper.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if !errors.As(err, &notFound) {
			return appError{code: exitInvalidConfig, err: fmt.Errorf("read config: %w", err)}
		}
	}

	return nil
}

func openState() (*state.Store, error) {
	paths := config.Paths{
		Home: viper.GetString("home"),
		Root: viper.GetString("root"),
	}
	return state.Open(paths.StateDB())
}

func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var app appError
	if errors.As(err, &app) {
		return app.code
	}
	_, _ = fmt.Fprintln(os.Stderr, err)
	return exitGeneric
}
