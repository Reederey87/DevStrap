package cli

// devstrap wip push|fetch <project> — working-state validation plane Layer B
// (spec/07 § Working-state plane). Push captures a git stash-create commit
// and pushes it to this device's own refs/devstrap/wip/<device_id>/<path_key>
// ref; fetch mirrors OTHER devices' WIP refs for a project into local refs
// without materializing anything. Strictly separate from agent worktree-base
// resolution — this plane must never be read by worktree.go's fresh-worktree
// resolver.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	dsgit "github.com/Reederey87/DevStrap/internal/git"
	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
	"github.com/spf13/cobra"
)

func newWipCommand(stdout io.Writer, opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "wip",
		Short: "Manage WIP (work-in-progress) recovery refs",
	}
	cmd.AddCommand(newWipPushCommand(stdout, opts))
	cmd.AddCommand(newWipFetchCommand(stdout, opts))
	return cmd
}

// wipRefFor derives the canonical WIP ref for a device+project. The ref is
// always derived this way — from a locally-trusted device id and the
// project's own path_key — and never taken from a stored/peer-supplied Ref
// string, so a peer can never redirect a fetch at an arbitrary ref via its
// synced device_wip mirror row.
func wipRefFor(deviceID, pathKey string) string {
	return "refs/devstrap/wip/" + deviceID + "/" + pathKey
}

type wipPushResult struct {
	Path   string `json:"path"`
	Ref    string `json:"ref,omitempty"`
	SHA    string `json:"sha,omitempty"`
	Pushed bool   `json:"pushed"`
}

func newWipPushCommand(stdout io.Writer, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "push <project>",
		Short: "Capture and push the working tree's uncommitted state to a recovery ref",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			project, err := store.ProjectByPath(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if project.Type != "git_repo" || project.LocalPath == "" {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("%s has no local git working tree to push from", project.Path)}
			}
			r := gitRunner(opts)
			sha, ok, err := r.StashCreate(cmd.Context(), project.LocalPath)
			if err != nil {
				return appError{code: exitGit, err: err}
			}
			if !ok {
				out := wipPushResult{Path: project.Path}
				return opts.render(stdout, func(w io.Writer) error {
					_, err := fmt.Fprintf(w, "Nothing to push for %s (working tree is clean)\n", project.Path)
					return err
				}, out)
			}
			baseSHA, err := r.RevParse(cmd.Context(), project.LocalPath, "HEAD")
			if err != nil {
				return appError{code: exitGit, err: err}
			}
			device, err := store.CurrentDevice(cmd.Context())
			if err != nil {
				return err
			}
			ref := wipRefFor(device.ID, project.PathKey)
			// wip push is an explicit, single-shot user command (unlike the
			// automatic per-sync-cycle gitstate capture): pushing twice with no
			// intervening commit produces two ref updates — a new stash-create
			// commit each time, even when its content is identical — since there
			// is no debounce against a previous push here. Accepted ref churn,
			// not a bug: the ref is a mutable pointer, not a history log.
			if err := r.PushRef(cmd.Context(), project.LocalPath, "origin", sha, ref); err != nil {
				return appError{code: exitGit, err: err}
			}
			payload := dssync.WipPayload{
				Path:       project.Path,
				Ref:        ref,
				SHA:        sha,
				BaseSHA:    baseSHA,
				CapturedAt: state.TimestampNow(),
			}
			raw, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			if err := store.WithTx(cmd.Context(), func(tx *state.Tx) error {
				// The emitting device must see its own pushed WIP state
				// immediately: ApplyEvents dedups an event ID already present
				// locally, so this device's own repo.wip.pushed event never
				// re-applies when pulled back from the hub. Mirroring it here,
				// in the SAME transaction as the event insert, is what makes
				// `wip status`/`doctor` on this device see the push right away.
				ev, err := store.InsertLocalEventTx(cmd.Context(), tx, dssync.NewWipPushedEvent(string(raw)))
				if err != nil {
					return err
				}
				return tx.UpsertDeviceWipTx(cmd.Context(), device.ID, project.PathKey, project.Path, state.WipParams{
					Ref:        ref,
					SHA:        sha,
					BaseSHA:    baseSHA,
					CapturedAt: payload.CapturedAt,
				}, ev)
			}); err != nil {
				return err
			}
			out := wipPushResult{Path: project.Path, Ref: ref, SHA: sha, Pushed: true}
			return opts.render(stdout, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "Pushed WIP for %s to %s (%s)\n", project.Path, ref, shortSHA(sha))
				return err
			}, out)
		},
	}
}

type wipFetchEntry struct {
	DeviceID   string `json:"device_id"`
	DeviceName string `json:"device_name,omitempty"`
	Ref        string `json:"ref"`
}

type wipFetchResult struct {
	Path    string          `json:"path"`
	Fetched []wipFetchEntry `json:"fetched,omitempty"`
}

func newWipFetchCommand(stdout io.Writer, opts *options) *cobra.Command {
	var deviceID string
	cmd := &cobra.Command{
		Use:   "fetch <project>",
		Short: "Fetch other devices' pushed WIP recovery refs for a project",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			project, err := store.ProjectByPath(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if project.Type != "git_repo" || project.LocalPath == "" {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("%s has no local git working tree to fetch into", project.Path)}
			}
			r := gitRunner(opts)

			if deviceID != "" {
				ref := wipRefFor(deviceID, project.PathKey)
				if err := r.FetchRef(cmd.Context(), project.LocalPath, "origin", ref); err != nil {
					if errors.Is(err, dsgit.ErrBranchNotFound) {
						out := wipFetchResult{Path: project.Path}
						return opts.render(stdout, func(w io.Writer) error {
							_, err := fmt.Fprintf(w, "No WIP ref found for device %s on %s\n", deviceID, project.Path)
							return err
						}, out)
					}
					return appError{code: exitGit, err: err}
				}
				out := wipFetchResult{Path: project.Path, Fetched: []wipFetchEntry{{DeviceID: deviceID, Ref: ref}}}
				return opts.render(stdout, func(w io.Writer) error {
					_, err := fmt.Fprintf(w, "Fetched %s (%s)\n", ref, deviceID)
					return err
				}, out)
			}

			// No --device: discover candidates from the local device_wip mirror
			// (populated by synced repo.wip.pushed events), never from our own
			// network fetch — this command only fetches refs the mirror already
			// says exist.
			rows, err := store.DeviceWipForProject(cmd.Context(), project.PathKey)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				out := wipFetchResult{Path: project.Path}
				return opts.render(stdout, func(w io.Writer) error {
					_, err := fmt.Fprintf(w, "No pending WIP known for %s\n", project.Path)
					return err
				}, out)
			}
			names, err := deviceNames(cmd.Context(), store)
			if err != nil {
				return err
			}
			var fetched []wipFetchEntry
			for _, row := range rows {
				ref := wipRefFor(row.DeviceID, project.PathKey)
				if err := r.FetchRef(cmd.Context(), project.LocalPath, "origin", ref); err != nil {
					if errors.Is(err, dsgit.ErrBranchNotFound) {
						continue
					}
					return appError{code: exitGit, err: err}
				}
				fetched = append(fetched, wipFetchEntry{DeviceID: row.DeviceID, DeviceName: names[row.DeviceID], Ref: ref})
			}
			out := wipFetchResult{Path: project.Path, Fetched: fetched}
			return opts.render(stdout, func(w io.Writer) error {
				if len(fetched) == 0 {
					_, err := fmt.Fprintf(w, "No pending WIP known for %s\n", project.Path)
					return err
				}
				for _, f := range fetched {
					label := f.DeviceID
					if f.DeviceName != "" {
						label = f.DeviceName
					}
					if _, err := fmt.Fprintf(w, "Fetched %s (%s)\n", f.Ref, label); err != nil {
						return err
					}
				}
				return nil
			}, out)
		},
	}
	cmd.Flags().StringVar(&deviceID, "device", "", "fetch only this device's WIP ref")
	return cmd
}

func deviceNames(ctx context.Context, store *state.Store) (map[string]string, error) {
	devices, err := store.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	names := make(map[string]string, len(devices))
	for _, d := range devices {
		names[d.ID] = d.Name
	}
	return names, nil
}
