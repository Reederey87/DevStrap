package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/hub"
	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
	"github.com/Reederey87/DevStrap/internal/workspacekeys"
	"github.com/spf13/cobra"
)

// hubFromOptions is the single Hub-selection seam (P5-HUB-01 / ARCH-03). Every
// hub-using command (sync, run-loop, devices revoke, hub gc, doctor --remote)
// resolves its Hub here instead of hardcoding dssync.FileHub, so the R2/S3
// backend is a one-line factory addition rather than a change at every call
// site. It returns the Hub plus a stable hub id used to key per-hub sync
// cursors.
//
// P4-SEC-02/SEC-07: the returned Hub is an EncryptedHub decorator wrapping the
// backend (FileHub or R2Hub) so the hub stores only ciphertext for the event
// log. The keyring is built lazily — EncryptedHub only touches it on Push/Pull
// of events, so blob-only paths (hub gc, doctor reachability) never need a
// bootstrapped epoch.
//
// Resolution order: the --hub-file flag, then a "hub" config value
// (file:<path>, r2://<bucket>, or s3://<bucket>). The R2/S3 backend
// keying/retry/conditional-put/GC logic lives in internal/hub; this seam builds
// the production aws-sdk-go-v2 S3Adapter, reads the workspace id (so hub cursors
// key on "r2:"+workspace_id, anticipated by migration 00008), and returns an
// R2Hub with the default retry policy.
func hubFromOptions(ctx context.Context, opts *options, store *state.Store, hubFile string) (dssync.Hub, string, error) {
	backend, hubID, err := selectBackendHub(ctx, opts, store, hubFile)
	if err != nil {
		return nil, "", err
	}
	return dssync.EncryptedHub{
		Hub:     backend,
		Keyring: buildKeyring(opts, store),
		Verify:  store.VerifyRemoteEvent,
		// P6-SEC-02: the founder/join gate in runSyncCycle reads RawSeen to
		// tell an empty hub (found) from a populated hub a joiner cannot yet
		// decrypt (wait for grant).
		Stats: &dssync.PullStats{},
	}, hubID, nil
}

// buildKeyring constructs the WCK epoch keyring (P4-SEC-07) from the state store
// and the local device key custody store. It is cheap (stores refs only); the
// keychain is read lazily on first Prime/IngestGrant/GrantAllEpochs.
func buildKeyring(opts *options, store *state.Store) *workspacekeys.Keyring {
	keyStore := devicekeys.NewHybridStore(opts.paths().KeyDir(), platform.Detect().Keychain)
	return workspacekeys.New(store, keyStore)
}

// selectBackendHub resolves the raw backend Hub (FileHub or R2Hub) without the
// encryption decorator. hubFromOptions wraps its result in EncryptedHub.
func selectBackendHub(ctx context.Context, opts *options, store *state.Store, hubFile string) (dssync.Hub, string, error) {
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
		return hubSpec{}, fmt.Errorf("unrecognized hub %q (want r2://<bucket> or s3://<bucket>)", u.Redacted())
	}
	if u.Host == "" {
		return hubSpec{}, fmt.Errorf("hub uri %q has no bucket", u.Redacted())
	}
	if u.User != nil {
		return hubSpec{}, fmt.Errorf("hub uri %q must not contain credentials", u.Redacted())
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
	var graceWindow time.Duration
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Reclaim hub blobs no longer referenced by any binding or draft snapshot (P5-HUB-02)",
		Long: `Reclaim hub blobs no longer referenced by any binding or draft snapshot.

gc first pulls and applies the hub event log so the mark set includes every
device's latest snapshots, and refuses to sweep while any pulled event was
deferred, skipped, or quarantined (P6-HUB-01) — a truncated view of the log
must never drive deletion. Blobs younger than --grace-window are kept even
when unreferenced, closing the race with a device that has pushed a blob
whose referencing event is not on the hub yet.

Run gc from one designated device; concurrent sweeps from several devices
are not coordinated.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			hub, hubID, err := hubFromOptions(cmd.Context(), opts, store, hubFile)
			if err != nil {
				return appError{code: exitInvalidConfig, err: err}
			}
			pruned, removed, err := hubGC(cmd.Context(), cmd.ErrOrStderr(), store, hub, hubID, keep, graceWindow, dryRun)
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
	cmd.Flags().DurationVar(&graceWindow, "grace-window", 24*time.Hour, "keep unreferenced blobs younger than this, so an in-flight push from another device is never swept")
	return cmd
}

// errGCRefused wraps the P6-HUB-01 refuse-to-sweep condition so tests can
// assert on it and callers exit non-zero without deleting anything.
var errGCRefused = errors.New("hub gc: refusing to sweep")

// hubGC reclaims superseded blobs (P5-HUB-02). It first syncs the namespace
// map (pull + apply) so the mark set reflects every device's latest events,
// refuses to sweep when that view is incomplete (P6-HUB-01), then prunes
// superseded draft snapshot rows (keeping the latest `keep` per project) so
// their blobs become unreferenced, lists every blob on the hub, and deletes
// those no retained secret binding or draft snapshot references — except blobs
// younger than grace, which are kept even when unreferenced (a device pushes
// its blob BEFORE its referencing event, so a fresh blob may be legitimately
// reference-less for one push cycle). It is the hub-side counterpart to
// gcUnreferencedBlobs (which only reclaims the LOCAL cache). A dry run prunes
// nothing but uses RetainedBlobRefs so the preview reflects post-prune state and
// matches what a real run would delete (P5 review).
func hubGC(ctx context.Context, stderr io.Writer, store *state.Store, hub dssync.Hub, hubID string, keep int, grace time.Duration, dryRun bool) (pruned, removed int, err error) {
	// P6-HUB-01 gate 1: sweep only from a fully-synced replica. The pull also
	// applies other devices' draft.snapshot.created events so their blobs
	// enter RetainedBlobRefs below.
	pull, err := pullAndApplyEvents(ctx, store, hub, hubID)
	if err != nil {
		return 0, 0, fmt.Errorf("pre-gc sync: %w", err)
	}
	// P6-HUB-01 gate 2: refuse to sweep on any signal that this device's view
	// of the event log is incomplete — a truncated/skipped pull (grant not yet
	// held, undecryptable events), a quarantined or cursor-holding apply, or
	// any still-open quarantine conflict from an earlier cycle. Sweeping from
	// a partial mark set deletes other devices' live blobs.
	if eh, ok := hub.(dssync.EncryptedHub); ok && eh.Stats != nil {
		if eh.Stats.Truncated > 0 || eh.Stats.Skipped > 0 {
			return 0, 0, appError{code: exitInvalidConfig, err: fmt.Errorf(
				"%w: pull deferred %d and skipped %d event(s); this device cannot see the full event log (awaiting a key grant or holding undecryptable events) — resolve that and re-run",
				errGCRefused, eh.Stats.Truncated, eh.Stats.Skipped)}
		}
	}
	if pull.stats.Quarantined > 0 || pull.stats.CursorHeld {
		return 0, 0, appError{code: exitInvalidConfig, err: fmt.Errorf(
			"%w: %d pulled event(s) were quarantined this cycle; run `devstrap conflicts list` and resolve before sweeping",
			errGCRefused, pull.stats.Quarantined)}
	}
	for _, typ := range dssync.QuarantineConflictTypes {
		open, cErr := store.OpenConflictsByType(ctx, typ)
		if cErr != nil {
			return 0, 0, cErr
		}
		if len(open) > 0 {
			return 0, 0, appError{code: exitInvalidConfig, err: fmt.Errorf(
				"%w: %d open %s conflict(s) mean unapplied events may reference blobs; run `devstrap conflicts list` and resolve before sweeping",
				errGCRefused, len(open), typ)}
		}
	}
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
	now := time.Now()
	for _, info := range hubBlobs {
		key := info.Key
		if referenced[key] {
			continue
		}
		// P6-HUB-01 gate 3: age grace window. A zero LastModified means the
		// backend could not report an age — treat it as young (keep) rather
		// than guess.
		if grace > 0 && (info.LastModified.IsZero() || now.Sub(info.LastModified) < grace) {
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
