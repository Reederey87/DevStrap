package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
	"github.com/spf13/cobra"
)

// hubFromOptions is the single Hub-selection seam (P5-HUB-01 / ARCH-03). Every
// hub-using command (sync, run-loop, devices revoke, hub gc) resolves its Hub
// here instead of hardcoding dssync.FileHub, so a future R2/S3 backend becomes a
// one-line factory addition rather than a change at every call site. It returns
// the Hub plus a stable hub id used to key per-hub sync cursors.
//
// Resolution order: the --hub-file flag, then a "hub" config value
// (file:<path> or r2://...). The R2/S3 backend keying/retry/conditional-put
// logic is implemented and unit-tested in internal/hub, but its production
// aws-sdk-go-v2 S3 client adapter is not yet wired into the binary, so r2://
// returns an actionable error for now (use --hub-file).
func hubFromOptions(opts *options, hubFile string) (dssync.Hub, string, error) {
	if hubFile == "" {
		hubFile = strings.TrimSpace(opts.v.GetString("hub-file"))
	}
	if hubFile != "" {
		return dssync.FileHub{Path: hubFile}, "file:" + hubFile, nil
	}
	uri := strings.TrimSpace(opts.v.GetString("hub"))
	if uri == "" {
		return nil, "", fmt.Errorf("no hub configured: pass --hub-file <path> or set 'hub' in config")
	}
	switch {
	case strings.HasPrefix(uri, "file:"):
		path := strings.TrimPrefix(uri, "file:")
		return dssync.FileHub{Path: path}, "file:" + path, nil
	case strings.HasPrefix(uri, "r2://"), strings.HasPrefix(uri, "s3://"):
		return nil, "", fmt.Errorf("the R2/S3 hub backend (%q) is not yet wired into this build (P5-HUB-01): internal/hub implements and unit-tests the R2Hub keying/retry/conditional-put logic, but the aws-sdk-go-v2 S3 client adapter and its MinIO integration test are the remaining step; use --hub-file for now", uri)
	default:
		return nil, "", fmt.Errorf("unrecognized hub %q (want file:<path> or r2://...)", uri)
	}
}

func newHubCommand(stdout io.Writer, opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hub",
		Short: "Operate on the sync hub (zero-knowledge event log + blob store)",
	}
	cmd.AddCommand(newHubGCCommand(stdout, opts))
	return cmd
}

func newHubGCCommand(stdout io.Writer, opts *options) *cobra.Command {
	var hubFile string
	var dryRun bool
	var keep int
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Reclaim hub blobs no longer referenced by any binding or draft snapshot (P5-HUB-02)",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			hub, _, err := hubFromOptions(opts, hubFile)
			if err != nil {
				return appError{code: exitInvalidConfig, err: err}
			}
			pruned, removed, err := hubGC(cmd.Context(), cmd.ErrOrStderr(), store, hub, keep, dryRun)
			if err != nil {
				return err
			}
			verb := "deleted"
			if dryRun {
				verb = "would delete"
			}
			_, err = fmt.Fprintf(stdout, "hub gc: pruned %d superseded draft snapshot(s); %s %d unreferenced hub blob(s)\n", pruned, verb, removed)
			return err
		},
	}
	cmd.Flags().StringVar(&hubFile, "hub-file", "", "file-backed test hub path")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "list what would be deleted without deleting")
	cmd.Flags().IntVar(&keep, "keep", 1, "draft snapshots to retain per project (>=1; the current snapshot is always kept)")
	return cmd
}

// hubGC reclaims superseded blobs (P5-HUB-02). It first prunes superseded draft
// snapshot rows (keeping the latest `keep` per project) so their blobs become
// unreferenced, then lists every blob on the hub and deletes those no retained
// secret binding or draft snapshot references. It is the hub-side counterpart to
// gcUnreferencedBlobs (which only reclaims the LOCAL cache). A dry run prunes
// nothing but uses RetainedBlobRefs so the preview reflects post-prune state and
// matches what a real run would delete (P5 review).
func hubGC(ctx context.Context, stderr io.Writer, store *state.Store, hub dssync.Hub, keep int, dryRun bool) (pruned, removed int, err error) {
	if !dryRun {
		pruned, err = store.PruneDraftSnapshots(ctx, keep)
		if err != nil {
			return 0, 0, err
		}
	}
	hubBlobs, err := hub.ListBlobs(ctx)
	if err != nil {
		return pruned, 0, fmt.Errorf("list hub blobs: %w", err)
	}
	// RetainedBlobRefs is the post-prune referenced set: env binding refs + the
	// kept (top-`keep`) draft snapshot refs. Using it for both dry-run and real
	// run makes the dry-run preview accurate (a real run prunes first, after
	// which AllBlobRefs == RetainedBlobRefs).
	refs, err := store.RetainedBlobRefs(ctx, keep)
	if err != nil {
		return pruned, 0, err
	}
	referenced := make(map[string]bool, len(refs))
	for _, ref := range refs {
		if hash, herr := envBlobHash(ref); herr == nil {
			referenced[hash] = true
		}
	}
	for _, key := range hubBlobs {
		if referenced[key] {
			continue
		}
		if dryRun {
			_, _ = fmt.Fprintf(stderr, "would delete unreferenced hub blob %s\n", key)
			removed++
			continue
		}
		if delErr := hub.DeleteBlob(ctx, key); delErr != nil {
			_, _ = fmt.Fprintf(stderr, "warning: failed to delete hub blob %s: %v\n", key, delErr)
			continue
		}
		removed++
	}
	return pruned, removed, nil
}
