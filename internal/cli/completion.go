package cli

import (
	"strings"

	"github.com/spf13/cobra"
)

// completePaths returns a cobra completion function that suggests namespace
// paths from the local store (P5-DX-01). It is the highest-value completion for
// a managed-namespace CLI: the same paths exist on every device, so tab-completing
// them from the DB makes the CLI feel native. It only completes the first
// positional argument and never falls back to file completion.
func completePaths(opts *options) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) >= 1 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		store, err := opts.openState(cmd.Context())
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		defer closeStore(store)
		projects, err := store.ListProjects(cmd.Context())
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		var out []string
		for _, p := range projects {
			if toComplete == "" || strings.HasPrefix(p.Path, toComplete) {
				out = append(out, p.Path)
			}
		}
		return out, cobra.ShellCompDirectiveNoFileComp
	}
}

// completeEnum returns a completion function suggesting a fixed set of values
// for an enum flag (P5-DX-01).
func completeEnum(values ...string) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		return values, cobra.ShellCompDirectiveNoFileComp
	}
}
