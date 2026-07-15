package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

// newUpCommand implements `devstrap up [root]` (P7-PROD-01 slice 2): a founder-
// side one-shot bootstrap that folds `init` (+ optional `scan --adopt`) +
// hub configuration + `sync` into one command for the FIRST device standing up
// a brand-new workspace.
//
// It is a thin SEQUENTIAL orchestrator over the existing commands' internal
// logic (runInit, rewriteConfigHub, runSyncCycle) — NOT a new atomic multi-step
// transaction. Each step is already independently idempotent and safe to
// interrupt, so a failure at any step leaves the prior steps in place and a
// re-run of `devstrap up` (or the single failed step, e.g. `devstrap sync`)
// continues from where it left off. `up` never undoes a completed step.
func newUpCommand(stdout io.Writer, opts *options) *cobra.Command {
	var hubURL string
	var scanAdopt bool
	var workspaceName string
	cmd := &cobra.Command{
		Use:   "up [root]",
		Short: "Bootstrap a new workspace in one step: init + scan + hub + sync (P7-PROD-01)",
		Args:  usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			stderr := cmd.ErrOrStderr()
			hub := strings.TrimSpace(hubURL)
			if hub == "" {
				return appError{code: exitUsage, err: fmt.Errorf("--hub is required: the hub to configure (a git carrier git@host:path.git / git+ssh://…, or r2://<bucket> / s3://<bucket>)")}
			}
			// Preflight the hub URI shape BEFORE founding anything so a typo fails
			// fast with no half-initialized workspace. Setting it in viper both
			// drives the shared hubConfigured validator (which reads opts.v — the
			// same validation the manual `hub init` / `hub:` config path uses,
			// including rejecting a git carrier's embedded credentials) and makes
			// the value visible to the in-process sync step below; rewriteConfigHub
			// persists it to config.yaml afterward for later standalone runs.
			opts.v.Set("hub", hub)
			if err := hubConfigured(opts, ""); err != nil {
				return appError{code: exitUsage, err: fmt.Errorf("invalid --hub %q: %w", hub, err)}
			}

			// Step 1 (init + optional scan/adopt): found the workspace and this
			// device's key identity. Idempotent — a re-run over an initialized home
			// is a no-op that leaves config untouched.
			if err := runInit(cmd, args, stdout, opts, initParams{
				workspaceName: workspaceName,
				scanAdopt:     scanAdopt,
				calledFromUp:  true,
			}); err != nil {
				return err
			}

			// Step 2 (hub): persist the hub into config.yaml via the surgical
			// rewrite that preserves every other line. Done BEFORE sync so that if
			// sync fails, a bare `devstrap sync` re-run already has the hub and
			// finishes from here.
			paths := opts.paths()
			if err := rewriteConfigHub(paths, hub); err != nil {
				opts.progressf(stderr, "up: the workspace was initialized (safe to keep); only writing the hub config failed. Re-run 'devstrap up'.\n")
				return err
			}
			opts.progressf(stderr, "Configured hub: %s\n", hub)

			// Step 3 (sync): on an empty hub this founds the workspace's key epoch
			// (the P6-SEC-02 founder gate lives inside runSyncCycle) and pushes the
			// namespace map. Surface sync's own error UNWRAPPED — a hub-unreachable
			// failure keeps the exact error and exit class sync already produces;
			// `up` is a thin wrapper, not a new error-handling layer.
			if err := runSyncCycle(cmd.Context(), stdout, opts, "", false, false); err != nil {
				opts.progressf(stderr, "up: init and hub configuration already succeeded and are safe to keep; only the final sync failed. Re-run 'devstrap up' — or just 'devstrap sync' — to finish from here.\n")
				return err
			}

			// Closing summary: name the founded workspace so the operator can copy
			// it and knows exactly what just happened.
			wsID := ""
			if store, serr := opts.openState(cmd.Context()); serr == nil {
				if w, ierr := store.WorkspaceID(cmd.Context()); ierr == nil {
					wsID = w
				}
				closeStore(store)
			}
			if wsID != "" {
				opts.progressf(stdout, "\nWorkspace up: %s founded, hub %s configured, initial sync complete.\n", wsID, hub)
			} else {
				opts.progressf(stdout, "\nWorkspace up: hub %s configured, initial sync complete.\n", hub)
			}
			opts.progressf(stdout, "Pair a second device: run 'devstrap pair' here (guided), then 'devstrap join <code>' on the other device.\n")
			return nil
		},
	}
	cmd.Flags().StringVar(&hubURL, "hub", "", "hub to configure: a git carrier (git@host:path.git, git+ssh://…) or r2://<bucket> / s3://<bucket> (required)")
	cmd.Flags().BoolVar(&scanAdopt, "scan", true, "scan the root and adopt existing repos during init")
	cmd.Flags().StringVar(&workspaceName, "workspace-name", "", "workspace name")
	return cmd
}
