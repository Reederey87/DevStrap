package cli

import (
	"encoding/json"
	"fmt"
	"io"

	dssync "github.com/Reederey87/DevStrap/internal/sync"
	"github.com/spf13/cobra"
)

func newConflictsCommand(stdout io.Writer, opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "conflicts",
		Short: "Inspect and resolve namespace conflicts (PROD-06)",
		// `devstrap conflicts` with no subcommand lists open conflicts so the
		// existing invocation keeps working.
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConflictsList(cmd, stdout, opts)
		},
	}
	cmd.AddCommand(newConflictsListCommand(stdout, opts))
	cmd.AddCommand(newConflictsShowCommand(stdout, opts))
	cmd.AddCommand(newConflictsResolveCommand(stdout, opts))
	return cmd
}

func newConflictsListCommand(stdout io.Writer, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List open namespace conflicts",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConflictsList(cmd, stdout, opts)
		},
	}
}

func runConflictsList(cmd *cobra.Command, stdout io.Writer, opts *options) error {
	store, err := opts.openState(cmd.Context())
	if err != nil {
		return err
	}
	defer closeStore(store)
	conflicts, err := store.OpenConflicts(cmd.Context())
	if err != nil {
		return err
	}
	if opts.v.GetBool("json") {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(conflicts)
	}
	if len(conflicts) == 0 {
		_, err = fmt.Fprintln(stdout, "No open conflicts.")
		return err
	}
	_, _ = fmt.Fprintf(stdout, "%d open conflict(s):\n\n", len(conflicts))
	for _, c := range conflicts {
		_, _ = fmt.Fprintf(stdout, "ID: %s\nType: %s\n", c.ID, c.Type)
		if c.NamespaceID != "" {
			_, _ = fmt.Fprintf(stdout, "Project: %s\n", c.NamespaceID)
		}
		_, _ = fmt.Fprintf(stdout, "Details: %s\n\n", c.DetailsJSON)
	}
	_, _ = fmt.Fprintln(stdout, "Resolve with: devstrap conflicts resolve <id> --keep-local|--keep-remote|--keep-both")
	return nil
}

func newConflictsShowCommand(stdout io.Writer, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show one conflict's details and status",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			c, err := store.ConflictByID(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if opts.v.GetBool("json") {
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(c)
			}
			_, _ = fmt.Fprintf(stdout, "ID: %s\nType: %s\nStatus: %s\n", c.ID, c.Type, c.Status)
			if c.NamespaceID != "" {
				_, _ = fmt.Fprintf(stdout, "Project: %s\n", c.NamespaceID)
			}
			_, _ = fmt.Fprintf(stdout, "Details: %s\n", c.DetailsJSON)
			return nil
		},
	}
}

func newConflictsResolveCommand(stdout io.Writer, opts *options) *cobra.Command {
	var keepLocal, keepRemote, keepBoth bool
	cmd := &cobra.Command{
		Use:   "resolve <id>",
		Short: "Resolve a namespace conflict (keep-local | keep-remote | keep-both)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			action, err := resolveAction(keepLocal, keepRemote, keepBoth)
			if err != nil {
				return appError{code: exitUsage, err: err}
			}
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			c, err := store.ConflictByID(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if c.Status != "open" {
				return appError{code: exitConflict, err: fmt.Errorf("conflict %s is already %s", c.ID, c.Status)}
			}
			resolution := map[string]string{
				"action":        action,
				"conflict_id":   c.ID,
				"conflict_type": c.Type,
			}
			if c.NamespaceID != "" {
				resolution["namespace_id"] = c.NamespaceID
			}
			raw, err := json.Marshal(resolution)
			if err != nil {
				return err
			}
			if err := store.ResolveConflict(cmd.Context(), c.ID, string(raw)); err != nil {
				return err
			}
			// Audit the decision as an HLC event so every device converges on
			// the same outcome and the open-conflict count stays down (PROD-06).
			if _, err := dssync.CreateConflictResolvedEvent(cmd.Context(), store, dssync.ConflictResolvedPayload{
				ConflictID:  c.ID,
				NamespaceID: c.NamespaceID,
				Type:        c.Type,
				Action:      action,
			}); err != nil {
				return fmt.Errorf("record conflict.resolved event: %w", err)
			}
			_, _ = fmt.Fprintf(stdout, "Conflict %s resolved (%s).\n", c.ID, action)
			if action == "keep-both" {
				_, _ = fmt.Fprintln(stdout, "Kept the local entry; re-add the remote variant under a sibling path (e.g. <path>.remote) if you want both to coexist.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&keepLocal, "keep-local", false, "keep the local version, discard the remote variant")
	cmd.Flags().BoolVar(&keepRemote, "keep-remote", false, "keep the remote version, discard the local variant")
	cmd.Flags().BoolVar(&keepBoth, "keep-both", false, "keep both (dual-copy): local stays, remote re-added under a sibling path")
	return cmd
}

// resolveAction validates that exactly one keep-* flag is set and returns the
// canonical action string (PROD-06).
func resolveAction(keepLocal, keepRemote, keepBoth bool) (string, error) {
	set := 0
	action := ""
	if keepLocal {
		set++
		action = "keep-local"
	}
	if keepRemote {
		set++
		action = "keep-remote"
	}
	if keepBoth {
		set++
		action = "keep-both"
	}
	if set == 0 {
		return "", fmt.Errorf("pass exactly one of --keep-local, --keep-remote, --keep-both")
	}
	if set > 1 {
		return "", fmt.Errorf("pass exactly one of --keep-local, --keep-remote, --keep-both (not several)")
	}
	return action, nil
}
