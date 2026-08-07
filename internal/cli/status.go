package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/cobra"
)

// statusPromptTimeout bounds `status --prompt`'s local SQLite reads (W12-01):
// generous relative to the local-only <50ms design target, but still a hard
// ceiling so a wedged store degrades a shell prompt segment rather than
// hanging the interactive terminal it's embedded in.
const statusPromptTimeout = 2 * time.Second

func newStatusCommand(stdout io.Writer, opts *options) *cobra.Command {
	var watch bool
	var interval time.Duration
	var allDevices bool
	var prompt bool
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
			// W12-01: --prompt is a distinct, terse renderer meant to be
			// embedded in a shell prompt (via `devstrap shell-init`) —
			// local-only, no network I/O, one line, never JSON-wrapped.
			if prompt {
				return renderStatusPrompt(cmd.Context(), stdout, opts)
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
	cmd.Flags().BoolVar(&prompt, "prompt", false, "print a single-line, machine-parseable summary for embedding in a shell prompt (local-only, no network I/O; see `devstrap shell-init`)")
	cmd.MarkFlagsMutuallyExclusive("prompt", "watch")
	cmd.MarkFlagsMutuallyExclusive("prompt", "all-devices")
	return cmd
}

// renderStatusPrompt implements `status --prompt` (W12-01): a fast,
// local-only, single-line summary meant to sit inside a shell prompt segment.
// It reads only already-synced local mirror state (never shells out to git,
// never touches the network, never triggers a sync) so it stays safely under
// a prompt's latency budget. The format is the terse "key:count" contract
// documented on the --prompt flag and in docs/quickstart.md, not prose.
//
// Failure contract: every early return below happens before the single
// Fprintln at the end, so on ANY error stdout stays completely empty (only
// the returned error carries detail, printed to stderr by the normal CLI
// error path) — never a partial line or a multi-line error dump. This is
// what makes `"$(devstrap status --prompt 2>/dev/null)"` in the emitted
// shell-init hooks safe to embed directly: a failure degrades to an empty
// $DEVSTRAP_PROMPT, never garbage in the prompt.
func renderStatusPrompt(ctx context.Context, stdout io.Writer, opts *options) error {
	// A shell prompt hook fires before every prompt render, so an unbounded
	// wait here (a wedged file lock, a slow/degraded disk) would hang the
	// interactive terminal itself, not just this one command. Bound it well
	// above the local-only <50ms target so ordinary contention never trips
	// it, but low enough that a truly stuck store still fails fast.
	ctx, cancel := context.WithTimeout(ctx, statusPromptTimeout)
	defer cancel()

	store, err := opts.openState(ctx)
	if err != nil {
		return err
	}
	defer closeStore(store)

	projects, err := store.ListProjects(ctx)
	if err != nil {
		return err
	}
	var dirty int
	for _, p := range projects {
		if p.DirtyState == "dirty" || p.DirtyState == "diverged" {
			dirty++
		}
	}
	// Workspace-wide pending WIP (working-state validation plane Layer B),
	// not filtered to the local device: a WIP ref pending from ANY device is
	// exactly the "something needs reconciling" signal a prompt should flag,
	// mirroring what `wip status` itself surfaces.
	wip, err := store.DeviceWipAll(ctx)
	if err != nil {
		return err
	}
	conflicts, err := store.CountOpenConflicts(ctx)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintln(stdout, formatPromptLine(dirty, len(wip), conflicts))
	return err
}

// formatPromptLine renders the `status --prompt` one-liner: "clean" when
// nothing needs attention, or space-joined "key:count" segments — dirty, wip,
// conflicts, in that priority order — for whichever are nonzero. A pure
// function (no store, no I/O) so the segment-selection logic is directly
// unit-testable.
func formatPromptLine(dirty, wip, conflicts int) string {
	var parts []string
	if dirty > 0 {
		parts = append(parts, fmt.Sprintf("dirty:%d", dirty))
	}
	if wip > 0 {
		parts = append(parts, fmt.Sprintf("wip:%d", wip))
	}
	if conflicts > 0 {
		parts = append(parts, fmt.Sprintf("conflicts:%d", conflicts))
	}
	if len(parts) == 0 {
		return "clean"
	}
	return strings.Join(parts, " ")
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
	// WIP is the compact pending-WIP summary (working-state validation plane
	// Layer B) alongside the Layer A gitstate columns above. Unlike Devices,
	// it has no forced empty-state row: zero pending WIP is the normal,
	// healthy case for most projects most of the time, so it is simply
	// omitted rather than rendered as an explicit "none" entry.
	WIP []wipStatusRow `json:"wip,omitempty"`
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
		var wip []wipStatusRow
		if wipRows, werr := store.DeviceWipForProject(ctx, project.PathKey); werr == nil {
			wip = wipRowsForProject(wipRows, now)
		}
		out = append(out, projectGitstateStatus{Path: project.Path, Devices: gitstateRowsForProject(rows, err, now), WIP: wip})
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
			if len(p.WIP) > 0 {
				parts := make([]string, 0, len(p.WIP))
				for _, wip := range p.WIP {
					parts = append(parts, fmt.Sprintf("%s (%s)", wip.DeviceID, wip.Observed))
				}
				_, _ = fmt.Fprintf(w, "  pending WIP for %s: %s\n", p.Path, strings.Join(parts, ", "))
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
