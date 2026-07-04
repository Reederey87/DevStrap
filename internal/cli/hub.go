package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Reederey87/DevStrap/internal/childenv"
	"github.com/Reederey87/DevStrap/internal/config"
	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/hub"
	"github.com/Reederey87/DevStrap/internal/redact"
	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
	"github.com/Reederey87/DevStrap/internal/workspacekeys"
	"github.com/spf13/cobra"
	"golang.org/x/term"
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
		Keyring: buildKeyring(ctx, opts, store),
		Verify:  store.VerifyRemoteEvent,
		// P6-SEC-02: the founder/join gate in runSyncCycle reads RawSeen to
		// tell an empty hub (found) from a populated hub a joiner cannot yet
		// decrypt (wait for grant).
		Stats: &dssync.PullStats{},
		// P6-SEC-03: bound the missing-grant truncate. Within the grace
		// window a not-yet-granted (epoch, kid) defers the pull tail (retry
		// next sync); past it the events are quarantined as undecryptable so
		// the cursor advances — recoverable later via the replay path once a
		// grant arrives.
		MissingKeyWait: store.NoteMissingKeyGrant,
		GraceWindow:    keyGrantGrace(opts),
		// P6-SYNC-02: durable records for events the pull must drop (unknown
		// envelope version, retired enc.v1, anti-downgrade plaintext) — the
		// visibility layer for the per-device retry wedge the seq gap causes.
		// The same grace window bounds the recoverable unknown-version defer.
		NoteSkipped: store.NoteSkippedEvent,
	}, hubID, nil
}

// hubCredStore builds the device key custody store for the hub S3 credential
// slot (P6-HUB-02), stamped with the recorded custody backend (P6-XP-04) so a
// file-custody machine never lets a stale keychain entry shadow the file slot.
// Reading the recorded decision is best-effort (an unreadable decision leaves
// legacy hybrid custody); DEVSTRAP_NO_KEYCHAIN still forces file custody.
func hubCredStore(ctx context.Context, opts *options, store *state.Store) devicekeys.HybridStore {
	base := devicekeys.NewHybridStore(opts.paths().KeyDir(), keychainBackend())
	var custody devicekeys.Custody
	if store != nil {
		custody, _ = store.KeyCustody(ctx)
	}
	return base.WithCustody(state.EffectiveKeyCustody(custody))
}

// buildKeyring constructs the WCK epoch keyring (P4-SEC-07) from the state store
// and the local device key custody store. It is cheap (one meta read for the
// recorded custody backend, then stores refs only); the keychain is read lazily
// on first Prime/IngestGrant/GrantAllEpochs. Stamping the recorded custody here
// (P6-XP-04) is best-effort: an unreadable decision leaves the store in the
// legacy hybrid mode, and DEVSTRAP_NO_KEYCHAIN still forces file custody.
func buildKeyring(ctx context.Context, opts *options, store *state.Store) *workspacekeys.Keyring {
	return buildKeyringFromPaths(ctx, opts.paths(), store)
}

// buildKeyringFromPaths builds the WCK epoch keyring from an explicit paths value
// rather than *options, for callers (hubGC's snapshot recovery) that hold paths
// but not opts.
func buildKeyringFromPaths(ctx context.Context, paths config.Paths, store *state.Store) *workspacekeys.Keyring {
	base := devicekeys.NewHybridStore(paths.KeyDir(), keychainBackend())
	custody, _ := store.KeyCustody(ctx)
	return workspacekeys.New(store, base.WithCustody(state.EffectiveKeyCustody(custody)))
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
		creds, err := resolveHubS3Credentials(ctx, opts, store, ws)
		if err != nil {
			return nil, "", err
		}
		adapter, err := hub.NewS3Client(endpoint, region, spec.bucket, creds.accessKeyID, creds.secret.Reveal())
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

// hubS3Creds is a resolved hub credential pair (P6-HUB-02). The secret is
// wrapped in redact.Secret from the moment it is resolved; the single Reveal()
// is the hub.NewS3Client call in selectBackendHub.
//
// NOTE: redact.Secret's own Stringer does NOT protect this struct — Go's
// reflection-based fmt cannot dispatch a Stringer on an UNEXPORTED field, so
// `%+v` of a bare hubS3Creds would dump the raw secret. The String/GoString/
// LogValue methods below exist to close exactly that hole; never print the
// struct through any other path (post-#45 review, gpt-5.5).
type hubS3Creds struct {
	accessKeyID string
	secret      redact.Secret
	source      string // "env/config", "op", or "keychain" — for diagnostics/tests; never the values
}

func (c hubS3Creds) String() string {
	return fmt.Sprintf("hubS3Creds{accessKeyID:%s, secret:%s, source:%s}", c.accessKeyID, redact.Placeholder, c.source)
}

func (c hubS3Creds) GoString() string { return c.String() }

func (c hubS3Creds) LogValue() slog.Value { return slog.StringValue(c.String()) }

// resolveHubS3Credentials resolves the S3/R2 credential pair for the hub
// (P6-HUB-02). Resolution order, most explicit first:
//
//  1. DEVSTRAP_HUB_S3_ACCESS_KEY_ID / DEVSTRAP_HUB_S3_SECRET_ACCESS_KEY
//     (env or config). A value with the op:// prefix is resolved through the
//     1Password CLI (`op read`); anything else is the literal credential —
//     the plaintext-env fallback CI depends on.
//  2. AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY (literal only).
//  3. The per-workspace keychain/file custody slot written by
//     `devstrap hub login` (devicekeys.HybridStore, DEVSTRAP_NO_KEYCHAIN-aware).
//
// The secret decides the source: an explicit secret wins even when the
// keychain also holds a pair (12-factor override), but a keychain pair only
// fills in whichever half is not explicitly set.
func resolveHubS3Credentials(ctx context.Context, opts *options, store *state.Store, workspaceID string) (hubS3Creds, error) {
	accessKeyID := strings.TrimSpace(opts.v.GetString("hub_s3_access_key_id"))
	if accessKeyID == "" {
		accessKeyID = strings.TrimSpace(os.Getenv("AWS_ACCESS_KEY_ID"))
	}
	secret := strings.TrimSpace(opts.v.GetString("hub_s3_secret_access_key"))
	if secret == "" {
		secret = strings.TrimSpace(os.Getenv("AWS_SECRET_ACCESS_KEY"))
	}
	source := "env/config"
	if strings.HasPrefix(accessKeyID, "op://") {
		resolved, err := resolveOpRef(ctx, accessKeyID)
		if err != nil {
			return hubS3Creds{}, fmt.Errorf("resolve hub s3 access key id: %w", err)
		}
		accessKeyID = resolved.Reveal()
		source = "op"
	}
	// The secret is carried as redact.Secret from the moment it is known; the
	// op:// branch must NOT return early — the keychain fill for a missing
	// access key id and the final validation below apply to every path
	// (post-#45 review Major, gpt-5.5: the early return silently broke the
	// keychain-access-key + op://-secret combination and skipped the
	// two-remedy error).
	var resolvedSecret redact.Secret
	if strings.HasPrefix(secret, "op://") {
		resolved, err := resolveOpRef(ctx, secret)
		if err != nil {
			return hubS3Creds{}, fmt.Errorf("resolve hub s3 secret access key: %w", err)
		}
		resolvedSecret = resolved
		source = "op"
	} else if secret != "" {
		resolvedSecret = redact.New(secret)
	}
	if resolvedSecret.IsZero() || accessKeyID == "" {
		stored, err := hubCredStore(ctx, opts, store).LoadHubS3Credentials(ctx, workspaceID)
		switch {
		case err == nil:
			if accessKeyID == "" {
				accessKeyID = stored.AccessKeyID
			}
			if resolvedSecret.IsZero() && stored.SecretAccessKey != "" {
				resolvedSecret = redact.New(stored.SecretAccessKey)
				source = "keychain"
			}
		case !errors.Is(err, os.ErrNotExist):
			return hubS3Creds{}, fmt.Errorf("load stored hub s3 credentials: %w", err)
		}
	}
	if resolvedSecret.IsZero() || accessKeyID == "" {
		return hubS3Creds{}, fmt.Errorf("no hub S3 credentials: set DEVSTRAP_HUB_S3_ACCESS_KEY_ID and DEVSTRAP_HUB_S3_SECRET_ACCESS_KEY (values may be 1Password op:// refs), or store them once with 'devstrap hub login' (see spec/19_CLOUD_PROVISIONING_GUIDE.md)")
	}
	return hubS3Creds{accessKeyID: accessKeyID, secret: resolvedSecret, source: source}, nil
}

// opReadTimeout bounds a single `op read` credential resolution, long enough
// for an interactive biometric/desktop-app approval but finite so a wedged
// prompt cannot hold a sync cycle open forever. A var so tests can shrink it.
var opReadTimeout = 60 * time.Second

// resolveOpRef resolves a single 1Password op://vault/item/field reference to
// its secret value via `op read` (P6-HUB-02), mirroring the provider path
// `devstrap run`/`env hydrate` use for env profiles. The subprocess runs under
// the sanitized child environment (BasicAllowlist + OP_*), stderr is captured
// for diagnostics, and the value is wrapped in redact.Secret immediately.
func resolveOpRef(ctx context.Context, ref string) (redact.Secret, error) {
	if _, err := exec.LookPath("op"); err != nil {
		return redact.Secret{}, fmt.Errorf("op:// refs require the 1Password CLI (`op`) on PATH: %w", err)
	}
	env, err := childenv.FromOS(append(childenv.BasicAllowlist(), "OP_*"), nil)
	if err != nil {
		return redact.Secret{}, err
	}
	// Bound the external call (CodeRabbit, PR #45): the CLI's root context is
	// signal-cancellable but has no deadline, and `op read` can block on an
	// interactive unlock prompt — without a local timeout that would stall
	// sync/run-loop indefinitely. 60s accommodates a biometric/desktop-app
	// approval; WaitDelay force-kills a child that ignores the cancel.
	octx, cancel := context.WithTimeout(ctx, opReadTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(octx, "op", "read", "--no-newline", ref) //nolint:gosec // fixed 1Password CLI command; ref is the operator-configured op:// reference and the env is sanitized.
	cmd.Env = env
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		return redact.Secret{}, fmt.Errorf("op read failed (is the 1Password CLI signed in?): %w: %s", err, redact.Scrub(strings.TrimSpace(stderr.String())))
	}
	value := strings.TrimSpace(stdout.String())
	if value == "" {
		return redact.Secret{}, fmt.Errorf("op read returned an empty value for the credential reference")
	}
	return redact.New(value), nil
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
	cmd.AddCommand(newHubLoginCommand(stdout, opts))
	cmd.AddCommand(newHubLogoutCommand(stdout, opts))
	return cmd
}

func newHubLoginCommand(stdout io.Writer, opts *options) *cobra.Command {
	var accessKeyID string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Store the hub S3/R2 credentials in the recorded custody backend (OS keychain, or the 0600 file store under file custody) (P6-HUB-02/P6-XP-04)",
		Long: `Store the hub S3/R2 access key id and secret access key in the OS keychain
(0600 file fallback when no keychain is available), so sync needs no plaintext
credential env vars. The secret is read from an interactive no-echo prompt, or
from stdin when piped — it is never accepted as a command-line argument
(argv leaks into process listings and shell history).

Explicit DEVSTRAP_HUB_S3_*/AWS_* env values still override the stored pair,
and either value may instead be a 1Password op:// reference resolved at sync
time. Remove stored credentials with 'devstrap hub logout'.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			ws, err := store.WorkspaceID(cmd.Context())
			if err != nil {
				return err
			}
			// One buffered reader for all prompts: a second bufio.Reader over
			// the same piped stdin would lose lines the first one buffered.
			reader := bufio.NewReader(cmd.InOrStdin())
			if accessKeyID == "" {
				value, err := promptLine(cmd, reader, "Access key id: ")
				if err != nil {
					return err
				}
				accessKeyID = value
			}
			secret, err := promptSecret(cmd, reader, "Secret access key (hidden): ")
			if err != nil {
				return err
			}
			if accessKeyID == "" || secret.IsZero() {
				return appError{code: exitUsage, err: fmt.Errorf("hub login: access key id and secret access key are both required")}
			}
			if strings.HasPrefix(secret.Reveal(), "op://") || strings.HasPrefix(accessKeyID, "op://") {
				return appError{code: exitUsage, err: fmt.Errorf("hub login stores literal credentials; keep op:// refs in DEVSTRAP_HUB_S3_* env/config instead — they resolve at sync time")}
			}
			keys := hubCredStore(cmd.Context(), opts, store)
			location, err := keys.StoreHubS3Credentials(cmd.Context(), ws, devicekeys.HubS3Credentials{
				AccessKeyID:     accessKeyID,
				SecretAccessKey: secret.Reveal(),
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(stdout, "Stored hub S3 credentials for workspace %s in the %s store.\n", ws, location)
			return err
		},
	}
	cmd.Flags().StringVar(&accessKeyID, "access-key-id", "", "S3/R2 access key id (not secret; the secret is always prompted or piped)")
	return cmd
}

func newHubLogoutCommand(stdout io.Writer, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove the stored hub S3/R2 credentials (P6-HUB-02)",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			ws, err := store.WorkspaceID(cmd.Context())
			if err != nil {
				return err
			}
			keys := hubCredStore(cmd.Context(), opts, store)
			if err := keys.DeleteHubS3Credentials(cmd.Context(), ws); err != nil {
				return err
			}
			_, err = fmt.Fprintf(stdout, "Removed stored hub S3 credentials for workspace %s.\n", ws)
			return err
		},
	}
}

// promptLine reads one non-secret line from the shared reader, printing the
// prompt only when stdin is a terminal. Used for the access key id when
// --access-key-id is not given.
func promptLine(cmd *cobra.Command, reader *bufio.Reader, prompt string) (string, error) {
	if f, ok := cmd.InOrStdin().(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		_, _ = fmt.Fprint(cmd.ErrOrStderr(), prompt)
	}
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("read input: %w", err)
	}
	return strings.TrimSpace(line), nil
}

// promptSecret reads the secret without echo when stdin is a terminal, and as
// a plain line from the shared reader when piped (scripting/tests). The value
// is wrapped in redact.Secret immediately and never accepted via argv.
func promptSecret(cmd *cobra.Command, reader *bufio.Reader, prompt string) (redact.Secret, error) {
	if f, ok := cmd.InOrStdin().(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		_, _ = fmt.Fprint(cmd.ErrOrStderr(), prompt)
		raw, err := term.ReadPassword(int(f.Fd()))
		_, _ = fmt.Fprintln(cmd.ErrOrStderr())
		if err != nil {
			return redact.Secret{}, fmt.Errorf("read secret: %w", err)
		}
		return redact.New(strings.TrimSpace(string(raw))), nil
	}
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return redact.Secret{}, fmt.Errorf("read secret from stdin: %w", err)
	}
	return redact.New(strings.TrimSpace(line)), nil
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

gc first pulls and applies the hub event log (fetching referenced blobs, like
sync) so the mark set includes every device's latest snapshots, and refuses to
sweep while any pulled event was deferred, skipped, or quarantined (P6-HUB-01)
— a truncated view of the log must never drive deletion. Blobs younger than
--grace-window are kept even when unreferenced, bounding the race with a
device that has pushed a blob whose referencing event is not on the hub yet:
a device offline longer than the window is not protected (it re-pushes on its
next successful sync).

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
			pruned, removed, err := hubGC(cmd.Context(), cmd.ErrOrStderr(), store, hub, hubID, opts.paths(), keep, graceWindow, dryRun)
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
func hubGC(ctx context.Context, stderr io.Writer, store *state.Store, hub dssync.Hub, hubID string, paths config.Paths, keep int, grace time.Duration, dryRun bool) (pruned, removed int, err error) {
	// P6-HUB-01 gate 1: sweep only from a fully-synced replica. The pull also
	// applies other devices' draft.snapshot.created events so their blobs
	// enter RetainedBlobRefs below.
	pull, err := pullAndApplyEvents(ctx, store, hub, hubID)
	if errors.Is(err, dssync.ErrSnapshotRequired) {
		// P4-SYNC-02: recover via snapshot exchange before sweeping, so the mark
		// set reflects the full compacted state. A keyless device cannot build a
		// complete view, so it must refuse rather than sweep from a partial one.
		_, _ = fmt.Fprintln(stderr, "Recovering from hub snapshot (retention floor passed our cursor)…")
		imported, rerr := recoverFromSnapshot(ctx, stderr, store, hub, hubID, paths, buildKeyringFromPaths(ctx, paths, store))
		if rerr != nil {
			return 0, 0, rerr
		}
		if !imported {
			return 0, 0, appError{code: exitInvalidConfig, err: fmt.Errorf(
				"%w: hub requires a snapshot but this device holds no workspace key to unseal it; approve this device and sync before running gc", errGCRefused)}
		}
		pull, err = pullAndApplyEvents(ctx, store, hub, hubID)
		if errors.Is(err, dssync.ErrSnapshotRequired) {
			return 0, 0, appError{code: exitNetwork, err: fmt.Errorf("pre-gc sync: hub still demands a snapshot after recovery — re-run devstrap sync")}
		}
	}
	if err != nil {
		return 0, 0, fmt.Errorf("pre-gc sync: %w", err)
	}
	// Cache the referenced blobs the pre-pull consumed, exactly as sync does —
	// the cursor has advanced past these events, so a later sync will never
	// see them again and this is the only chance to fetch their blobs.
	if missing, bErr := pullReferencedBlobs(ctx, hub, pull.events, paths); bErr != nil {
		return 0, 0, appError{code: exitNetwork, err: fmt.Errorf("pre-gc sync: pull blobs: %w", bErr)}
	} else if missing > 0 {
		_, _ = fmt.Fprintf(stderr, "warning: %d referenced blob(s) missing from hub; materialization may be incomplete\n", missing)
	}
	// P6-HUB-01 gate 2: refuse to sweep on any signal that this device's view
	// of the event log is incomplete — a truncated/skipped pull (grant not yet
	// held, undecryptable events), a quarantined or cursor-holding apply, or
	// any still-open quarantine conflict from an earlier cycle. Sweeping from
	// a partial mark set deletes other devices' live blobs.
	if eh, ok := hub.(dssync.EncryptedHub); ok && eh.Stats != nil {
		// Undecryptable carriers also land in pull.stats.Quarantined below;
		// this counter is checked too so the refusal message names the cause.
		if eh.Stats.Truncated > 0 || eh.Stats.Skipped > 0 || eh.Stats.Undecryptable > 0 {
			return 0, 0, appError{code: exitInvalidConfig, err: fmt.Errorf(
				"%w: pull deferred %d, skipped %d, and quarantined %d undecryptable event(s); this device cannot see the full event log (awaiting a key grant or holding undecryptable events) — resolve that and re-run",
				errGCRefused, eh.Stats.Truncated, eh.Stats.Skipped, eh.Stats.Undecryptable)}
		}
	}
	// P6-SYNC-02: the durable skip table outlives one pull's in-memory stats —
	// a record from an EARLIER cycle (unknown envelope version awaiting an
	// upgrade, retired v1, anti-downgrade plaintext) still means this device's
	// view is incomplete even when the current pull happened to see nothing.
	skipped, sErr := store.OpenSkippedEvents(ctx)
	if sErr != nil {
		// Fail CLOSED: this gate exists to stop a sweep from an incomplete
		// view, so an unreadable skip table must abort like the quarantine
		// gate below, never silently proceed (CodeRabbit, PR #63).
		return 0, 0, fmt.Errorf("read skipped events before sweep: %w", sErr)
	}
	if len(skipped) > 0 {
		return 0, 0, appError{code: exitInvalidConfig, err: fmt.Errorf(
			"%w: %d event(s) remain skipped (see `devstrap doctor`); the hub is serving objects this device cannot consume yet — upgrade devstrap or investigate, then re-run",
			errGCRefused, len(skipped))}
	}
	if pull.stats.CursorHeld {
		// A transiently-held event (clock skew or hash-chain break) is
		// re-delivered on a later pull; `conflicts resolve` cannot clear the
		// hold, so point at the actual remedy.
		return 0, 0, appError{code: exitInvalidConfig, err: fmt.Errorf(
			"%w: a pulled event is transiently held (clock skew or hash-chain break) and will be re-delivered; re-run gc after a later sync applies it",
			errGCRefused)}
	}
	if pull.stats.Quarantined > 0 {
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
