package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/cobra"
)

func newStatusCommand(stdout io.Writer, opts *options) *cobra.Command {
	var watch bool
	var interval time.Duration
	var allDevices bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show local workspace status",
		RunE: func(cmd *cobra.Command, args []string) error {
			// A missing state database means the workspace was never
			// initialized; surface the friendly guidance instead of a raw
			// sqlite "unable to open database file" error.
			if _, err := os.Stat(opts.paths().StateDB()); errors.Is(err, os.ErrNotExist) {
				return appError{code: exitInvalidConfig, err: state.ErrNotInitialized}
			}
			// P7-GITSTATE-01 CLI surfacing: --all-devices renders the
			// working-state validation plane Layer A mirror instead of the
			// regular snapshot; it does not compose with --watch.
			if allDevices {
				return renderAllDevicesStatus(cmd.Context(), stdout, opts)
			}
			if !watch {
				return renderStatus(cmd.Context(), stdout, opts)
			}
			// P5-PROD-05: live convergence view — re-render the readiness table,
			// open conflicts, and worktree/dirty state on an interval until the
			// user interrupts (Ctrl-C) or the scheduler stops the command.
			if interval <= 0 {
				interval = 2 * time.Second
			}
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			json := opts.v.GetBool("json")
			for {
				if !json {
					_, _ = fmt.Fprint(stdout, "\033[H\033[2J") // clear screen
				}
				if err := renderStatus(cmd.Context(), stdout, opts); err != nil {
					return err
				}
				if !json {
					_, _ = fmt.Fprintf(stdout, "\n(watching every %s — Ctrl-C to stop)\n", interval)
				}
				select {
				case <-cmd.Context().Done():
					return nil
				case <-ticker.C:
				}
			}
		},
	}
	cmd.Flags().BoolVar(&watch, "watch", false, "re-render status on an interval until interrupted (live convergence view)")
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "refresh interval for --watch")
	cmd.Flags().BoolVar(&allDevices, "all-devices", false, "show every device's last-observed git working-state per project (working-state validation plane Layer A)")
	return cmd
}

// renderStatus renders a single status snapshot (PROD-01/02), honoring --json.
func renderStatus(ctx context.Context, stdout io.Writer, opts *options) error {
	store, err := opts.openState(ctx)
	if err != nil {
		return err
	}
	defer closeStore(store)

	summary, err := store.Summary(ctx)
	if err != nil {
		if errors.Is(err, state.ErrNotInitialized) {
			return appError{code: exitInvalidConfig, err: err}
		}
		return err
	}

	// status --watch --json reuses this path each tick: one full JSON document
	// per render, no wrapper/delta framing (screen-clear stays outside when json).
	return opts.render(stdout, func(w io.Writer) error {
		// P4-SEC-07 pairing: surface the workspace id so a founder can copy it for
		// `init --join --workspace-id <id>` on a joining device, and so two devices
		// can eyeball-compare that they share one hub prefix.
		if _, err := fmt.Fprintf(w, "Workspace: %s\nWorkspace ID: %s\nRoot: %s\nProjects: %d\n", summary.WorkspaceName, summary.WorkspaceID, summary.RootPath, summary.ProjectCount); err != nil {
			return err
		}
		// PROD-02: surface open conflict count in status.
		if n, err := store.CountOpenConflicts(ctx); err == nil && n > 0 {
			_, _ = fmt.Fprintf(w, "Open conflicts: %d (run `devstrap conflicts` to inspect)\n", n)
		}
		// P6-SYNC-02: surface skipped hub events — objects this device's pulls
		// keep dropping (unknown envelope version, retired enc.v1, anti-downgrade
		// plaintext); each holds its origin device's cursor until it applies.
		if skipped, err := store.OpenSkippedEvents(ctx); err == nil && len(skipped) > 0 {
			_, _ = fmt.Fprintf(w, "Skipped hub events: %d (run `devstrap doctor` for reasons)\n", len(skipped))
		}
		if len(summary.Projects) > 0 {
			_, _ = fmt.Fprintln(w, "\nProject\tType\tStatus\tDirty")
			for _, project := range summary.Projects {
				// PROD-01: derive a display status from the materialization and
				// dirty states instead of showing raw values.
				status := deriveDisplayStatus(project.MaterializationState, project.DirtyState)
				dirty := project.DirtyState
				if dirty == "" {
					dirty = "unknown"
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", project.Path, project.Type, status, dirty)
			}
			// P4-GIT-07: surface persisted materialize failure/warning text so
			// operators can see WHY a project failed (not just that it failed).
			var failed []state.ProjectStatus
			for _, project := range summary.Projects {
				if project.LastError != "" {
					failed = append(failed, project)
				}
			}
			if len(failed) > 0 {
				_, _ = fmt.Fprintln(w, "\nFailed materializations:")
				for _, project := range failed {
					_, _ = fmt.Fprintf(w, "  %s: %s\n", project.Path, project.LastError)
				}
			}
		}
		return nil
	}, summary)
}

// deviceGitstateRow is one device's observed working-state for a project, the
// synthetic "never synced" row for a project with zero device_gitstate rows,
// or an "error: ..." row when reading that one project's gitstate failed.
// spec/07 Layer A requires `status --all-devices` to always render an
// observed column — a project must never be silently omitted.
type deviceGitstateRow struct {
	DeviceID       string `json:"device_id,omitempty"`
	Branch         string `json:"branch,omitempty"`
	DirtyCount     int    `json:"dirty_count"`
	UntrackedCount int    `json:"untracked_count"`
	UnmergedCount  int    `json:"unmerged_count"`
	AheadCount     int    `json:"ahead_count"`
	BehindCount    int    `json:"behind_count"`
	StashCount     int    `json:"stash_count"`
	Observed       string `json:"observed"`
}

type projectGitstateStatus struct {
	Path    string              `json:"path"`
	Devices []deviceGitstateRow `json:"devices"`
}

// gitstateRowsForProject maps one project's DeviceGitstateForProject result
// to its rendered rows: an "error: ..." row when the read itself failed, an
// explicit "never synced" row when it succeeded with zero observations, or
// one row per device otherwise. Extracted as a pure function (no store, no
// I/O) so the error branch — which must never abort the surrounding
// per-project loop in renderAllDevicesStatus and blank out every other,
// already-successfully-read project — is directly unit-testable.
func gitstateRowsForProject(rows []state.DeviceGitstate, err error, now time.Time) []deviceGitstateRow {
	switch {
	case err != nil:
		return []deviceGitstateRow{{Observed: "error: " + err.Error()}}
	case len(rows) == 0:
		return []deviceGitstateRow{{Observed: "never synced"}}
	default:
		out := make([]deviceGitstateRow, 0, len(rows))
		for _, r := range rows {
			out = append(out, deviceGitstateRow{
				DeviceID:       r.DeviceID,
				Branch:         r.Branch,
				DirtyCount:     r.DirtyCount,
				UntrackedCount: r.UntrackedCount,
				UnmergedCount:  r.UnmergedCount,
				AheadCount:     r.AheadCount,
				BehindCount:    r.BehindCount,
				StashCount:     r.StashCount,
				Observed:       fmt.Sprintf("last seen %s ago", now.Sub(state.HLCPhysicalTime(r.ObservedAtHLC)).Round(time.Second)),
			})
		}
		return out
	}
}

// renderAllDevicesStatus implements `status --all-devices` (P7-GITSTATE-01
// CLI surfacing): for every local project it renders each device's
// last-observed git working-state, newest first, from the mirror-only
// device_gitstate table. A project no device has ever reported on gets one
// explicit "never synced" row instead of being left out of the output, and a
// project whose own gitstate read fails gets a visible "error: ..." row
// instead of aborting the whole render and blacking out every other,
// already-successfully-read project.
func renderAllDevicesStatus(ctx context.Context, stdout io.Writer, opts *options) error {
	store, err := opts.openState(ctx)
	if err != nil {
		return err
	}
	defer closeStore(store)

	projects, err := store.ListProjects(ctx)
	if err != nil {
		return err
	}

	now := time.Now()
	out := make([]projectGitstateStatus, 0, len(projects))
	for _, project := range projects {
		rows, err := store.DeviceGitstateForProject(ctx, project.PathKey)
		out = append(out, projectGitstateStatus{Path: project.Path, Devices: gitstateRowsForProject(rows, err, now)})
	}

	return opts.render(stdout, func(w io.Writer) error {
		if len(out) == 0 {
			_, _ = fmt.Fprintln(w, "No projects.")
			return nil
		}
		_, _ = fmt.Fprintln(w, "Project\tDevice\tBranch\tDirty\tUntracked\tUnmerged\tAhead\tBehind\tStash\tObserved")
		for _, p := range out {
			for _, d := range p.Devices {
				device := d.DeviceID
				if device == "" {
					device = "-"
				}
				branch := d.Branch
				if branch == "" {
					branch = "-"
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%s\n",
					p.Path, device, branch, d.DirtyCount, d.UntrackedCount, d.UnmergedCount, d.AheadCount, d.BehindCount, d.StashCount, d.Observed)
			}
		}
		return nil
	}, out)
}

// deriveDisplayStatus maps the raw materialization and dirty states to a
// user-facing display label (PROD-01 / P5-PROD-01). It branches ONLY on the
// states writers actually produce — "skeleton", "available", "failed",
// "materialized-empty" — so the headline "ready" state (a materialized,
// clean checkout) is reachable. The earlier "hydrated"/"hydrating" branches
// were dead (no writer ever set those values). When env_ready/tooling_ready
// land, expand "ready" to require them too.
func deriveDisplayStatus(materialization, dirty string) string {
	switch materialization {
	case "failed":
		return "failed"
	case "skeleton":
		return "skeleton"
	case "materialized-empty":
		return "empty checkout"
	}
	// materialization == "available" (or any materialized value writers emit):
	// distinguish a clean checkout ("ready") from a dirty one.
	switch dirty {
	case "dirty", "diverged":
		return "dirty"
	case "clean":
		return "ready"
	default:
		return "available"
	}
}
