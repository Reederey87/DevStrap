package cli

import (
	"encoding/json"
	"fmt"

	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/spf13/cobra"
)

// exitSandboxHelper is the "wrapper itself failed" exit code (docker/git-bisect
// convention). The parent propagates child exits as childExitBase+N, so a shim
// failure surfaces as 225 — distinguishable from the reserved 1–10 CLI codes,
// and the shim's stderr lands in the parent-written 0600 agent log.
const exitSandboxHelper = 125

// newSandboxHelperCommand is the hidden Landlock re-exec shim behind the
// Linux fallback sandbox (P4-GIT-03 slice 3): parse the spec, restrict this
// process, execve the agent argv. Hidden commands are exempt from the
// command-doc drift gate; spec/13 documents the shim as internal anyway.
func newSandboxHelperCommand() *cobra.Command {
	var specJSON string
	cmd := &cobra.Command{
		// The first Use word is the command name; sourcing it from the
		// platform const keeps it in lockstep with the argv the adapter
		// renders.
		Use:    platform.SandboxHelperCommand + " --spec <json> -- command [args...]",
		Short:  "Internal landlock re-exec shim for agent run (not for direct use)",
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		// Root's PersistentPreRunE (initConfig + logging) must NOT run: the
		// shim executes with HOME repointed to the agent worktree, and config
		// init would materialize a stray ~/.devstrap inside it. A local no-op
		// wins over the root hook.
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(cmd *cobra.Command, args []string) error {
			var spec platform.SandboxSpec
			if err := json.Unmarshal([]byte(specJSON), &spec); err != nil {
				return appError{code: exitSandboxHelper, err: fmt.Errorf("sandbox-helper: parse --spec: %w", err)}
			}
			// Only returns on failure; on success the process image has been
			// replaced by the agent command.
			err := platform.ExecSandboxHelper(spec, args)
			return appError{code: exitSandboxHelper, err: fmt.Errorf("sandbox-helper: %w", err)}
		},
	}
	cmd.Flags().StringVar(&specJSON, "spec", "", "sandbox spec JSON (internal)")
	_ = cmd.MarkFlagRequired("spec")
	return cmd
}
