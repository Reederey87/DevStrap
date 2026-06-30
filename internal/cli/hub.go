package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/Reederey87/DevStrap/internal/hub"
	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
	"github.com/spf13/cobra"
)

// hubFromOptions is the single Hub-selection seam (P5-HUB-01 / ARCH-03). Every
// hub-using command (sync, run-loop, devices revoke, hub gc, doctor --remote)
// resolves its Hub here instead of hardcoding dssync.FileHub, so the R2/S3
// backend is a one-line factory addition rather than a change at every call
// site. It returns the Hub plus a stable hub id used to key per-hub sync
// cursors.
//
// Resolution order: the --hub-file flag, then a "hub" config value
// (file:<path>, r2://<bucket>, or s3://<bucket>). The R2/S3 backend
// keying/retry/conditional-put/GC logic lives in internal/hub; this seam builds
// the production aws-sdk-go-v2 S3Adapter, reads the workspace id (so hub cursors
// key on "r2:"+workspace_id, anticipated by migration 00008), and returns an
// R2Hub with the default retry policy.
func hubFromOptions(ctx context.Context, opts *options, store *state.Store, hubFile string) (dssync.Hub, string, error) {
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
		// P5-HUB-01: wire the live R2/S3 hub. The bucket (and optional
		// ?endpoint=&region= override) come from the URI; credentials and the
		// endpoint come from env/config (DEVSTRAP_HUB_S3_*), never the URI.
		spec, err := parseHubURI(uri)
		if err != nil {
			return nil, "", err
		}
		ws, err := store.WorkspaceID(ctx)
		if err != nil {
			if errors.Is(err, state.ErrNotInitialized) {
				return nil, "", fmt.Errorf("r2 hub requires an initialized workspace: run `devstrap init`")
			}
			return nil, "", err
		}
		endpoint := spec.endpoint
		if endpoint == "" {
			endpoint = strings.TrimSpace(opts.v.GetString("hub_s3_endpoint"))
		}
		if endpoint == "" {
			return nil, "", fmt.Errorf("r2 hub %q: no endpoint set (set ?endpoint= on the hub uri or DEVSTRAP_HUB_S3_ENDPOINT)", spec.bucket)
		}
		region := spec.region
		if region == "" {
			region = strings.TrimSpace(opts.v.GetString("hub_s3_region"))
		}
		if region == "" {
			region = "auto"
		}
		accessKeyID := strings.TrimSpace(opts.v.GetString("hub_s3_access_key_id"))
		if accessKeyID == "" {
			accessKeyID = os.Getenv("AWS_ACCESS_KEY_ID")
		}
		secretAccessKey := strings.TrimSpace(opts.v.GetString("hub_s3_secret_access_key"))
		if secretAccessKey == "" {
			secretAccessKey = os.Getenv("AWS_SECRET_ACCESS_KEY")
		}
		adapter, err := hub.NewS3Client(endpoint, region, spec.bucket, accessKeyID, secretAccessKey)
		if err != nil {
			return nil, "", err
		}
		// Hub-id keys per-hub sync cursors; "r2:"+ws is anticipated by migration
		// 00008. Zero Retry => R2Hub's default retry policy (HUB-10).
		return hub.R2Hub{S3: adapter, WorkspaceID: ws}, "r2:" + ws, nil
	default:
		return nil, "", fmt.Errorf("unrecognized hub %q (want file:<path> or r2://...)", uri)
	}
}

// hubSpec is the parsed r2:// or s3:// hub URI (P5-HUB-01).
type hubSpec struct {
	scheme   string
	bucket   string
	endpoint string
	region   string
}

// parseHubURI parses an r2://<bucket> or s3://<bucket> hub URI with optional
// ?endpoint=&region= query overrides. The bucket is the URI host. Credentials
// are NEVER carried in the URI (they come from DEVSTRAP_HUB_S3_* env/config).
// It is pure so it can be unit-tested hermetically.
func parseHubURI(uri string) (hubSpec, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return hubSpec{}, fmt.Errorf("parse hub uri: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "r2" && scheme != "s3" {
		return hubSpec{}, fmt.Errorf("unrecognized hub %q (want r2://<bucket> or s3://<bucket>)", uri)
	}
	if u.Host == "" {
		return hubSpec{}, fmt.Errorf("hub uri %q has no bucket", uri)
	}
	if u.User != nil {
		return hubSpec{}, fmt.Errorf("hub uri %q must not contain credentials", uri)
	}
	spec := hubSpec{scheme: scheme, bucket: u.Host}
	if e := u.Query().Get("endpoint"); e != "" {
		spec.endpoint = e
	}
	if r := u.Query().Get("region"); r != "" {
		spec.region = r
	}
	return spec, nil
}

// hubConfigured is a lightweight, store-free hub-config validator used by the
// run-loop preflight (P5-HUB-01): run-loop has no state store open at preflight
// time, so it cannot build the R2 adapter. It checks that a hub is resolvable
// (a file path or a well-formed r2:// URI) without touching the SDK or the DB.
func hubConfigured(opts *options, hubFile string) error {
	if hubFile == "" {
		hubFile = strings.TrimSpace(opts.v.GetString("hub-file"))
	}
	if hubFile != "" {
		return nil
	}
	uri := strings.TrimSpace(opts.v.GetString("hub"))
	if uri == "" {
		return fmt.Errorf("no hub configured: pass --hub-file <path> or set 'hub' in config")
	}
	switch {
	case strings.HasPrefix(uri, "file:"):
		return nil
	case strings.HasPrefix(uri, "r2://"), strings.HasPrefix(uri, "s3://"):
		if _, err := parseHubURI(uri); err != nil {
			return err
		}
		return nil
	default:
		return fmt.Errorf("unrecognized hub %q (want file:<path> or r2://...)", uri)
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
			hub, _, err := hubFromOptions(cmd.Context(), opts, store, hubFile)
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
