package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Reederey87/DevStrap/internal/config"
	"github.com/Reederey87/DevStrap/internal/logging"
	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

// blobPushConcurrency bounds how many referenced encrypted blobs sync uploads
// in parallel. Blobs are content-addressed and unordered, so the event-log
// ordering constraints do not apply to this fan-out (P6-HUB-03).
const blobPushConcurrency = 8

// syncResult is the --json shape for `sync` and `run-loop --once` (P5-CLI-01 part B).
// One shared type covers dry-run previews and real-cycle outcomes (hubCompactResult precedent).
type syncResult struct {
	HubID                 string   `json:"hub_id"`
	DryRun                bool     `json:"dry_run,omitempty"`
	WouldPush             int      `json:"would_push,omitempty"`
	Pushed                int      `json:"pushed,omitempty"`
	Pulled                int      `json:"pulled,omitempty"`
	Deferred              bool     `json:"deferred,omitempty"`
	NamespaceOnly         bool     `json:"namespace_only,omitempty"`
	KeyRotated            bool     `json:"key_rotated,omitempty"`
	BlobsGCd              int      `json:"blobs_gcd,omitempty"`
	MaterializedTotal     int      `json:"materialized_total,omitempty"`
	MaterializedSucceeded int      `json:"materialized_succeeded,omitempty"`
	MaterializedSkipped   int      `json:"materialized_skipped,omitempty"`
	Warnings              []string `json:"warnings,omitempty"`
}

func newSyncCommand(stdout io.Writer, opts *options) *cobra.Command {
	var hubFile string
	var namespaceOnly bool
	var dryRun bool
	var keyMaxAge string
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Push and pull namespace events and materialize the tree",
		RunE: func(cmd *cobra.Command, args []string) error {
			// P4-SEC-07: --key-max-age overrides keys.rotate_max_age for this
			// run only. Validated here so a typo is a usage error, not a
			// silent fallback.
			if strings.TrimSpace(keyMaxAge) != "" {
				d, err := time.ParseDuration(strings.TrimSpace(keyMaxAge))
				if err != nil {
					return appError{code: exitUsage, err: fmt.Errorf("invalid --key-max-age %q: %w", keyMaxAge, err)}
				}
				// ParseDuration accepts negatives; reject them here so the
				// flag's promise holds (a bad value is a usage error, never a
				// silent fallback to the default) — post-#56 opus review.
				if d < 0 {
					return appError{code: exitUsage, err: fmt.Errorf("invalid --key-max-age %q: must be >= 0 (0 disables auto-rotation)", keyMaxAge)}
				}
				opts.v.Set("keys.rotate_max_age", strings.TrimSpace(keyMaxAge))
			}
			return runSyncCycle(cmd.Context(), stdout, cmd.ErrOrStderr(), opts, hubFile, namespaceOnly, dryRun)
		},
	}
	cmd.Flags().StringVar(&hubFile, "hub-file", "", "file-backed test hub path")
	cmd.Flags().BoolVar(&namespaceOnly, "namespace-only", false, "sync namespace metadata only; skip materialization")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show sync plan without writing")
	cmd.Flags().StringVar(&keyMaxAge, "key-max-age", "", "override keys.rotate_max_age for this run (e.g. 720h; 0 disables auto-rotation)")
	return cmd
}

// runSyncCycle performs one sync + materialize cycle (EAGER-01/02, DRAFT-02).
// It is the reusable loop body shared by `devstrap sync` and `devstrap run-loop`
// (XP-02). dryRun prints the plan without writing.
//
// stderr carries process diagnostics (rotation narration, push-gate notices,
// snapshot-recovery progress, durability warnings) so --json stdout stays a
// single pure syncResult document (P5-CLI-01 part B).
func runSyncCycle(ctx context.Context, stdout, stderr io.Writer, opts *options, hubFile string, namespaceOnly, dryRun bool) error {
	store, err := opts.openState(ctx)
	if err != nil {
		return err
	}
	defer closeStore(store)
	// P5-HUB-01: resolve the hub through the single selection seam.
	hub, hubID, err := hubFromOptions(ctx, opts, store, hubFile)
	if err != nil {
		return appError{code: exitInvalidConfig, err: err}
	}
	// SYNC-04/P5-SYNC-01: the push watermark bounds the push side so a sync
	// cycle re-uploads only new local-origin events (Seq > push watermark),
	// not the entire event log including remote-origin events the hub already
	// holds from their origin device. Keyed by the gapless local Seq (a
	// "push:<hubID>" row in hub_device_cursors, backfilled once from the
	// legacy HLC watermark) so an HLC regression can never strand an event
	// behind the watermark.
	pushCursor, err := store.PushSeqCursor(ctx, hubID)
	if err != nil {
		return err
	}
	localEvents, err := store.LocalPendingEventsBySeq(ctx, pushCursor)
	if err != nil {
		return err
	}
	result := syncResult{HubID: hubID}
	if dryRun {
		// P6-CLI-05: print the resolved hub ID (file:<path> / r2:<ws…>), never
		// the raw --hub-file flag, which is empty when the hub comes from config.
		result.DryRun = true
		result.WouldPush = len(localEvents)
		return opts.render(stdout, func(w io.Writer) error {
			_, err := fmt.Fprintf(w, "Would push %d local events to %s and pull namespace events\n", result.WouldPush, result.HubID)
			return err
		}, result)
	}
	// P6-SEC-02: pull BEFORE push. A joining device must ingest its grant (and
	// a founding device must observe an empty hub) before it can decide whether
	// to found or wait, and a joiner must never push its pre-approval events
	// under a self-minted, never-granted WCK. The old order (push first) forced
	// a joiner's Push to either error on the missing epoch or seal events under
	// a key nobody else holds — the SEC-02 data loss.
	// EAGER-02: cursor-based incremental pull.
	pull, err := pullAndApplyEvents(ctx, store, hub, hubID)
	if errors.Is(err, dssync.ErrSnapshotRequired) {
		// P4-SYNC-02: the pull cursor fell behind the hub's retention floor.
		// Recover via a full-state snapshot exchange, then resume incremental
		// pulls (which now succeed because import advanced the cursors).
		// Mid-cycle progress is process diagnostics — route to stderr so --json
		// stdout stays pure (P5-CLI-01 part B).
		opts.progressf(stderr, "Recovering from hub snapshot (retention floor passed our cursor)…\n")
		imported, rerr := recoverFromSnapshot(ctx, stderr, store, hub, hubID, opts.paths(), buildKeyring(ctx, opts, store))
		if rerr != nil {
			return rerr
		}
		if !imported {
			// Keyless joiner: awaiting a grant. recoverFromSnapshot already
			// printed the defer message; nothing to materialize or push yet.
			return exportHubDurabilityAfterSync(ctx, stderr, opts, store, hub, hubFile, time.Now())
		}
		pull, err = pullAndApplyEvents(ctx, store, hub, hubID)
		if errors.Is(err, dssync.ErrSnapshotRequired) {
			return appError{code: exitNetwork, err: fmt.Errorf("hub still demands a snapshot after recovery — re-run devstrap sync")}
		}
	}
	if err != nil {
		return err
	}
	remoteEvents := pull.events
	// DRAFT-02: pull referenced blobs from the hub and cache them locally.
	missingBlobs, err := pullReferencedBlobs(ctx, hub, remoteEvents, opts.paths())
	if err != nil {
		return appError{code: exitNetwork, err: fmt.Errorf("pull blobs: %w", err)}
	}
	if missingBlobs > 0 {
		// Non-fatal warning rides syncResult.Warnings under --json (P7-CLI-01 /
		// P5-CLI-01); human mode still prints via fmt.Fprintln in the render callback.
		result.Warnings = append(result.Warnings, fmt.Sprintf("warning: %d referenced blob(s) missing from hub; materialization may be incomplete", missingBlobs))
	}

	// P4-SEC-07: age-triggered periodic WCK rotation. Deliberately AFTER the
	// pull — a freshly ingested grant resets the local epoch age, which
	// suppresses fleet-wide rotation storms (whichever device syncs first past
	// the deadline rotates; everyone else pulls the new epoch and stands down)
	// — and BEFORE the push, so the mint's grant events ride THIS cycle.
	// Rotation narration is process diagnostics on stderr — JSON only needs KeyRotated.
	rotated, rerr := maybeRotateWorkspaceKey(ctx, stderr, opts, store)
	if rerr != nil {
		return rerr
	}
	result.KeyRotated = rotated
	if rotated {
		// The localEvents snapshot above predates the mint; re-read so the
		// just-minted grant events are pushed in this same cycle.
		localEvents, err = store.LocalPendingEventsBySeq(ctx, pushCursor)
		if err != nil {
			return err
		}
	}
	rotationAccounted := rotated
	if !rotationAccounted {
		if _, owed, oerr := wckRotationPendingSince(ctx, store); oerr != nil {
			return oerr
		} else if owed {
			rotationAccounted = true
		} else if epoch, _, eerr := store.ActiveKeyEpochAge(ctx); eerr != nil {
			return eerr
		} else if epoch == 0 {
			rotationAccounted = true
		}
	}
	// Revoke-containment resume lines are process diagnostics (stderr) so
	// --json stdout stays a single document.
	if err := resumeRevokeContainment(ctx, stderr, opts, store, hub, rotationAccounted); err != nil {
		return err
	}

	// P6-SEC-02 founder/join gate. After the pull (which ingested any grant for
	// this device), decide whether this device holds a workspace key. If not,
	// either found the workspace (only when the hub is genuinely empty and this
	// device did not `init --join`) or defer the push until a grant arrives.
	rawSeen := len(remoteEvents)
	if eh, ok := hub.(dssync.EncryptedHub); ok && eh.Stats != nil {
		rawSeen = eh.Stats.RawSeen
	}
	// pushLocalEventsGated can emit awaiting-grant / drain-blob lines; keep them
	// off the result stream (same purity fix as hub_compact's call site).
	pushed, deferred, ferr := pushLocalEventsGated(ctx, stderr, opts, store, hub, hubID, localEvents, rawSeen)
	if ferr != nil {
		return ferr
	}

	// P4-SYNC-06: publish this device's signed sync ack after a fully-clean
	// cycle so a compactor can safely GC tombstones this device has already
	// consumed. Best-effort — a failure never fails the sync.
	maybeWriteSyncAck(ctx, store, hub, hubID, opts.paths(), pull, deferred)

	result.Pushed = pushed
	result.Pulled = len(remoteEvents)
	result.Deferred = deferred
	queuedLocal := len(localEvents)

	if namespaceOnly {
		if err := exportHubDurabilityAfterSync(ctx, stderr, opts, store, hub, hubFile, time.Now()); err != nil {
			return err
		}
		result.NamespaceOnly = true
		return opts.render(stdout, func(w io.Writer) error {
			for _, warning := range result.Warnings {
				_, _ = fmt.Fprintln(w, warning)
			}
			if result.Deferred {
				opts.progressf(w, "Synced namespace events: pulled %d; %d local event(s) queued awaiting workspace key grant\n", result.Pulled, queuedLocal)
				return nil
			}
			opts.progressf(w, "Synced namespace events: pushed %d, pulled %d\n", result.Pushed, result.Pulled)
			return nil
		}, result)
	}
	// EAGER-01: eager materialization.
	projects, err := store.SkeletonProjects(ctx)
	if err != nil {
		return err
	}
	// sync always materializes with a blobless/partial clone (EAGER-01).
	results := materializePass(ctx, store, opts, projects, true)
	// HUB-05: reclaim locally-cached blobs no longer referenced.
	if removed, gcErr := gcUnreferencedBlobs(ctx, store, opts.paths()); gcErr == nil && removed > 0 {
		result.BlobsGCd = removed
	}
	result.MaterializedTotal = results.total
	result.MaterializedSucceeded = results.succeeded
	result.MaterializedSkipped = results.skipped
	if err := exportHubDurabilityAfterSync(ctx, stderr, opts, store, hub, hubFile, time.Now()); err != nil {
		return err
	}
	return opts.render(stdout, func(w io.Writer) error {
		for _, warning := range result.Warnings {
			_, _ = fmt.Fprintln(w, warning)
		}
		if result.BlobsGCd > 0 {
			opts.progressf(w, "GC'd %d unreferenced blob(s)\n", result.BlobsGCd)
		}
		opts.progressf(w, "Synced events: pushed %d, pulled %d; materialized %d/%d projects (%d skipped)\n",
			result.Pushed, result.Pulled, result.MaterializedSucceeded, result.MaterializedTotal, result.MaterializedSkipped)
		return nil
	}, result)
}

// pullApplyOutcome reports one pull+apply pass: the decrypted events and the
// apply-side quarantine stats.
type pullApplyOutcome struct {
	events []state.Event
	stats  dssync.ApplyStats
}

// pullAndApplyEvents runs the pull half of a sync cycle: per-device
// cursor-based incremental pull, apply, and safe cursor advance. It is shared
// by runSyncCycle and hub gc's pre-GC sync gate (P6-HUB-01).
//
// SYNC-01/P5-SYNC-01: ApplyEvents returns a per-origin-device safe cursor —
// for each device, the end of the contiguous consumed run — never past a
// transiently-skipped (quarantined or hash-chain-broken) event or a seq gap.
// Advancing past such an event would permanently strand it, since Pull only
// returns Seq > cursor per device.
func pullAndApplyEvents(ctx context.Context, store *state.Store, hub dssync.Hub, hubID string) (pullApplyOutcome, error) {
	cursorMap, err := store.HubDeviceCursors(ctx, hubID)
	if err != nil {
		return pullApplyOutcome{}, err
	}
	cursor := dssync.Cursor(cursorMap)
	remoteEvents, err := hub.Pull(ctx, cursor)
	if err != nil {
		return pullApplyOutcome{}, err
	}
	// P6-SYNC-04 review fix: re-attempt previously-quarantined undecryptable
	// carriers with the keys held now (this pull may have ingested the grant
	// they were waiting for). Runs on every pull consumer (sync, run-loop,
	// hub gc's pre-GC sync), so a hub that mis-steered a not-yet-granted
	// event into quarantine by tampering with the unauthenticated kid hint
	// only delays that event until its grant lands — never loses it.
	// P6-SEC-03: the replay runs BEFORE this batch applies. A recovered
	// carrier is by construction an EARLIER event than anything in this batch
	// (its quarantine advanced the cursor past it), and the same origin
	// device's successors chain onto it via prev-hash — replaying first lets
	// a batch [recovered predecessor, successor] converge in ONE cycle instead
	// of quarantining the successor on a broken chain and waiting for the next
	// pull's re-delivery.
	if eh, ok := hub.(dssync.EncryptedHub); ok {
		if _, err := dssync.ReplayUndecryptableConflicts(ctx, store, eh); err != nil {
			return pullApplyOutcome{}, err
		}
	}
	safeCursor, stats, err := dssync.ApplyEventsWithStats(ctx, store, remoteEvents, cursor)
	if err != nil {
		return pullApplyOutcome{}, err
	}
	for dev, seq := range safeCursor {
		if seq > cursor.After(dev) {
			if err := store.AdvanceHubDeviceCursor(ctx, hubID, dev, seq); err != nil {
				return pullApplyOutcome{}, err
			}
		}
	}
	// Pointer events quarantined earlier because their project had not applied
	// yet may be recoverable now that this batch applied (the project could
	// have arrived in it). Replay AFTER apply so recovery is one-cycle.
	if _, err := dssync.ReplayPendingProjectConflicts(ctx, store); err != nil {
		return pullApplyOutcome{}, err
	}
	// P4-SYNC-05: check every approved peer's signed head against the prefix we
	// actually received, detecting a hub withholding a peer's newest events (or
	// a forked stream). Runs after the cursor has advanced so localSeq reflects
	// this batch. A hub read failure here is non-fatal to the sync (best-effort,
	// like the ack write) — it only means no omission check this cycle.
	if err := verifyPeerHeads(ctx, store, hub, hubID); err != nil {
		logging.Logger(ctx).Warn("sync: peer-head omission check failed (best-effort)", "err", err.Error())
	}
	return pullApplyOutcome{events: remoteEvents, stats: stats}, nil
}

// verifyPeerHeads runs the P4-SYNC-05 omission/fork detector for one pull. It
// gathers the local identity and delegates to dssync.VerifyPeerHeads, which
// records any event_omission conflict.
func verifyPeerHeads(ctx context.Context, store *state.Store, hub dssync.Hub, hubID string) error {
	_ = hubID // heads are workspace-scoped, not per-hub; kept for call-site symmetry
	workspaceID, err := store.WorkspaceID(ctx)
	if err != nil {
		return err
	}
	device, err := store.CurrentDevice(ctx)
	if err != nil {
		return err
	}
	_, err = dssync.VerifyPeerHeads(ctx, store, hub, workspaceID, device.ID)
	return err
}

// defaultKeyGrantGrace bounds how long a pull keeps deferring (truncating) on
// an event whose workspace key has not been granted to this device before the
// event is quarantined instead, unwedging the cursor (P6-SEC-03). 72h rides
// out a weekend-offline approver; an explicit 0 means quarantine immediately.
const defaultKeyGrantGrace = 72 * time.Hour

// keyGrantGrace resolves sync.key_grant_grace. Parsed here rather than via
// viper's GetDuration because GetDuration maps a malformed value to 0 — which
// would silently turn a typo into "quarantine immediately" (the same trap as
// materialization.clone_timeout, P6-GIT-01).
func keyGrantGrace(opts *options) time.Duration {
	if opts == nil || opts.v == nil {
		return defaultKeyGrantGrace
	}
	raw := strings.TrimSpace(opts.v.GetString("sync.key_grant_grace"))
	if raw == "" {
		return defaultKeyGrantGrace
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		fmt.Fprintf(os.Stderr, "warning: invalid sync.key_grant_grace %q; using default %s\n", raw, defaultKeyGrantGrace)
		return defaultKeyGrantGrace
	}
	return d
}

// defaultKeyRotateMaxAge is the periodic-rotation deadline for the workspace
// content key (P4-SEC-07): once the active epoch is older than this, the next
// sync on ANY device mints epoch+1 and grants it to all approved devices. 90
// days bounds forward exposure of a silently compromised key; an explicit 0
// disables age-triggered rotation.
const defaultKeyRotateMaxAge = 2160 * time.Hour

// keyRotateMaxAge resolves keys.rotate_max_age. Parsed here rather than via
// viper's GetDuration because GetDuration maps a malformed value to 0 — which
// would silently turn a typo into "rotation disabled" (the same trap as
// sync.key_grant_grace and materialization.clone_timeout).
func keyRotateMaxAge(opts *options) time.Duration {
	if opts == nil || opts.v == nil {
		return defaultKeyRotateMaxAge
	}
	raw := strings.TrimSpace(opts.v.GetString("keys.rotate_max_age"))
	if raw == "" {
		return defaultKeyRotateMaxAge
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		fmt.Fprintf(os.Stderr, "warning: invalid keys.rotate_max_age %q; using default %s\n", raw, defaultKeyRotateMaxAge)
		return defaultKeyRotateMaxAge
	}
	return d
}

// maybeRotateWorkspaceKey rotates the WCK when the active epoch is older than
// keys.rotate_max_age (P4-SEC-07 periodic rotation). At most one rotation per
// sync cycle; epoch 0 (keyless founder-to-be or ungranted joiner) is skipped —
// there is no key to age out, and a joiner must never self-mint (P6-SEC-02).
// Like `keys rotate`, this is a pure Rotate: no secret-rotation flags, no blob
// rewrap, no hub deletes — those are revoke semantics.
//
// Any device may rotate. The known residual (documented in spec/07/15): the
// rotator grants only to the approved devices it knows LOCALLY (the device
// registry is per-device), so a fleet device unknown to the rotator lands on
// the P6-SEC-03 grace→quarantine→replay path until any device that knows it
// re-approves it.
//
// Issue #134: it also retries a rotation OWED from a failed revoke-path
// rotation (the wck_rotation_pending marker). The owed retry runs even when
// keys.rotate_max_age=0 — disabling PERIODIC rotation must not disable the
// revoke containment a device already committed to.
func maybeRotateWorkspaceKey(ctx context.Context, stderr io.Writer, opts *options, store *state.Store) (bool, error) {
	pendingSince, pending, err := wckRotationPendingSince(ctx, store)
	if err != nil {
		return false, err
	}
	containmentDevices, containmentPending, _, err := revokeContainmentPending(ctx, store)
	if err != nil {
		return false, err
	}
	if !pending {
		for _, since := range containmentDevices {
			if !since.IsZero() && (pendingSince.IsZero() || since.Before(pendingSince)) {
				pendingSince = since
			}
		}
	}
	epoch, created, err := store.ActiveKeyEpochAge(ctx)
	if err != nil {
		return false, err
	}
	if epoch == 0 {
		return false, nil
	}
	maxAge := keyRotateMaxAge(opts)
	aged := maxAge > 0 && time.Since(created) >= maxAge
	if !pending && !containmentPending && !aged {
		return false, nil
	}
	kr := buildKeyring(ctx, opts, store)
	newEpoch, grants, rerr := kr.Rotate(ctx)
	if rerr != nil {
		// FATAL for the cycle when the failure could be a mid-commit
		// half-mint (post-#56 Codex review, P1): if the active epoch advanced
		// under this failed Rotate, pushing would seal this cycle's events
		// under an epoch whose grants never fully published. Detect it by
		// re-reading the epoch rather than assuming.
		if after, aerr := store.CurrentKeyEpoch(ctx); aerr != nil || after != epoch {
			return false, fmt.Errorf("workspace key rotation failed mid-commit (epoch advanced %d→%d; grants may not have published): %w", epoch, after, rerr)
		}
		if pending || containmentPending {
			if !pending {
				if merr := markWCKRotationPending(ctx, store, epoch); merr != nil {
					return false, fmt.Errorf("record workspace key rotation owed by pending revoke containment: %w", merr)
				}
				pendingSince = time.Now().UTC()
			}
			// Early failure (nothing recorded — the malformed-recipient
			// class). Do NOT fail the cycle: aborting here would also block
			// pushing the device.revoked event itself, so the fleet never
			// learns about the revoke — strictly worse than pushing under the
			// old epoch, which is the already-documented exposure
			// (adversarial-review finding, issue #134). Warn loudly, keep the
			// marker, retry next cycle; deliberately NOT progressf-gated.
			_, _ = fmt.Fprintf(stderr, "warning: workspace key rotation owed since %s still failing (events remain readable by the revoked device until it succeeds): %v\n",
				pendingSince.Format(time.RFC3339), rerr)
			_, _ = fmt.Fprintf(stderr, "warning: check 'devstrap devices list' for a malformed age recipient, or run 'devstrap keys rotate' after fixing it\n")
			return false, nil
		}
		// Age-triggered early failure keeps the shipped fatal semantics: a
		// malformed recipient row needs fixing, not retrying, and there is no
		// queued revoke event whose propagation the abort would block.
		return false, fmt.Errorf("periodic workspace key rotation failed (fix the cause or disable via keys.rotate_max_age=0 / --key-max-age 0): %w", rerr)
	}
	if pending || containmentPending {
		// The ONLY resolution path for the owed marker (see wck_rotation.go:
		// a newer epoch alone is not proof the revoked device was excluded; a
		// local Rotate is). A delete failure surfaces as a cycle error so the
		// marker cannot silently outlive its rotation.
		if pending {
			if cerr := clearWCKRotationPending(ctx, store); cerr != nil {
				return true, cerr
			}
		}
		if pendingSince.IsZero() {
			opts.progressf(stderr, "Rotated workspace key to epoch %d (rotation owed after a device revoke); %d grant event(s) ride this push\n",
				newEpoch, len(grants))
		} else {
			opts.progressf(stderr, "Rotated workspace key to epoch %d (rotation owed since %s after a device revoke); %d grant event(s) ride this push\n",
				newEpoch, pendingSince.Format(time.RFC3339), len(grants))
		}
		return true, nil
	}
	opts.progressf(stderr, "Rotated workspace key to epoch %d (epoch %d exceeded keys.rotate_max_age %s); %d grant event(s) ride this push\n",
		newEpoch, epoch, maxAge, len(grants))
	return true, nil
}

// pushLocalEventsGated runs the push side of a sync cycle behind the P6-SEC-02
// founder/join gate. It is called AFTER the pull (so any inbound grant for this
// device has already been ingested). rawSeen is the count of objects the pull
// observed on the hub (EncryptedHub.PullStats.RawSeen).
//
// Behavior:
//   - This device already holds a workspace key (epoch > 0): push normally.
//   - Epoch 0 and the hub is genuinely empty (rawSeen == 0 AND both the pull
//     and push cursors are 0 — this device has never observed hub content) and
//     this device did not `init --join`: FOUND the workspace — mint epoch 1 —
//     then push. This is the founding device's first sync.
//   - Epoch 0 otherwise (a joiner, or the hub already has content): DEFER. The
//     push cursor is left unadvanced so the queued local events re-push on a
//     later cycle once this device is approved and ingests the fleet WCK.
//
// Returns the number of events actually pushed and whether the push was
// deferred (mutually exclusive: deferred implies pushed == 0).
func pushLocalEventsGated(ctx context.Context, stderr io.Writer, opts *options, store *state.Store, hub dssync.Hub, hubID string, localEvents []state.Event, rawSeen int) (pushed int, deferred bool, err error) {
	kr := buildKeyring(ctx, opts, store)
	epoch, err := kr.CurrentEpoch(ctx)
	if err != nil {
		return 0, false, err
	}
	if epoch == 0 {
		// The cursors must be untouched too: rawSeen only counts objects
		// returned AFTER the current pull cursor, so on its own it proves
		// "nothing new", not "hub empty". A keyless device that previously
		// advanced any cursor (e.g. past events that all quarantined as
		// permanent verification failures) would otherwise see rawSeen == 0 on
		// a populated hub and wrongly found a divergent epoch-1 key.
		// P5-SYNC-01: check BOTH cursor tables — the per-device rows AND the
		// frozen legacy hub_cursors rows. A device that synced before the
		// per-device-cursor migration has no new-table rows; skipping the
		// legacy check would let it self-found and re-open the P6-SEC-02
		// split-brain.
		hasDeviceCursors, cerr := store.HasHubDeviceCursors(ctx, hubID)
		if cerr != nil {
			return 0, false, cerr
		}
		legacyPushCursor, cerr := store.HubCursor(ctx, "push:"+hubID)
		if cerr != nil {
			return 0, false, cerr
		}
		legacyPullCursor, cerr := store.HubCursor(ctx, hubID)
		if cerr != nil {
			return 0, false, cerr
		}
		neverSynced := !hasDeviceCursors && legacyPushCursor == 0 && legacyPullCursor == 0 && rawSeen == 0
		if neverSynced && !isJoiner(opts) {
			// Founder's first sync to an empty hub: mint epoch 1, then push.
			if _, berr := kr.EnsureBootstrap(ctx); berr != nil {
				return 0, false, fmt.Errorf("found workspace key: %w", berr)
			}
		} else {
			// Ungranted joiner (or empty-hub joiner): defer the push. Leaving the
			// push cursor unadvanced keeps localEvents queued for a later cycle.
			// Deliberately NOT gated by --quiet (unlike the summary lines above):
			// this is the only explanation of a real actionable state — an
			// unapproved device otherwise sees a silent no-op — and mirrors the
			// analogous awaiting-grant message in recoverFromSnapshot
			// (snapshot_recovery.go), which is likewise always visible.
			_, _ = fmt.Fprintf(stderr, "Awaiting workspace key grant: %d local event(s) queued. "+
				"On an approved device run 'devstrap devices enroll … --approve' (or 'devices approve <id>'), then re-run sync.\n", len(localEvents))
			return 0, true, nil
		}
	}

	// DRAFT-02: push local blobs referenced by pending events and the events
	// themselves in one hub batch. On the git carrier this is one commit/push;
	// object-store backends preserve their per-operation behavior.
	if err := hub.Batch(ctx, func(ops dssync.BatchOps) error {
		if err := pushReferencedBlobs(ctx, ops, localEvents, opts.paths()); err != nil {
			return fmt.Errorf("push blobs: %w", err)
		}
		return ops.Push(ctx, localEvents)
	}); err != nil {
		return 0, false, appError{code: exitNetwork, err: err}
	}
	// SYNC-04/P5-SYNC-01: advance the push watermark to the highest pushed
	// local Seq so the next cycle only pushes newly-originated events.
	if len(localEvents) > 0 {
		var maxPushSeq int64
		for _, e := range localEvents {
			if e.Seq > maxPushSeq {
				maxPushSeq = e.Seq
			}
		}
		if err := store.AdvancePushSeqCursor(ctx, hubID, maxPushSeq); err != nil {
			return 0, false, err
		}
	}
	// P5-PROD-02: now that local events (including any superseding rewrap event)
	// and referenced blobs are on the hub, drain blobs queued by a prior
	// local-only revoke so the old ciphertext is finally removed. This MUST stay
	// after the push so the superseding event is on the hub before its old
	// ciphertext is deleted.
	if drained, derr := drainPendingHubDeletes(ctx, store, hub); derr != nil {
		logging.Logger(ctx).Warn("sync: pending hub delete drain failed", "err", derr.Error())
	} else if drained > 0 {
		opts.progressf(stderr, "Removed %d superseded blob(s) from the hub\n", drained)
	}
	return len(localEvents), false, nil
}

// isJoiner reports whether this workspace was initialized with `init --join`
// (role: joiner in config.yaml). A joiner never founds a workspace on first
// sync (P6-SEC-02).
func isJoiner(opts *options) bool {
	return strings.EqualFold(strings.TrimSpace(opts.v.GetString("role")), "joiner")
}

// pushReferencedBlobs pushes locally-cached blobs referenced by events to the
// hub (DRAFT-02 blob plane).
func pushReferencedBlobs(ctx context.Context, hub dssync.BatchOps, events []state.Event, paths config.Paths) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(blobPushConcurrency)
	for _, event := range events {
		ref, ok := blobRefFromEvent(event)
		if !ok {
			continue
		}
		refCopy := ref
		g.Go(func() error {
			cached, err := readEnvBlob(paths, refCopy)
			if err != nil {
				return fmt.Errorf("push blob %s: cannot read local cache: %w", refCopy, err)
			}
			if err := hub.PutBlob(gctx, blobHashHex(refCopy), bytes.NewReader(cached)); err != nil {
				return fmt.Errorf("push blob %s: %w", refCopy, err)
			}
			return nil
		})
	}
	return g.Wait()
}

// pullReferencedBlobs fetches blobs referenced by remote events from the hub and
// caches them locally (DRAFT-02 blob plane).
func pullReferencedBlobs(ctx context.Context, hub dssync.Hub, events []state.Event, paths config.Paths) (int, error) {
	refs := make([]string, 0, len(events))
	for _, event := range events {
		if ref, ok := blobRefFromEvent(event); ok {
			refs = append(refs, ref)
		}
	}
	return pullBlobsByRef(ctx, hub, refs, paths)
}

// pullBlobsByRef fetches the given age_blob:<sha256> refs from the hub and caches
// them locally, returning the count that were missing or failed content-address
// verification. It backs both pullReferencedBlobs (refs from events) and snapshot
// recovery (refs from imported draft pointers, which have no carrier event on the
// hub tail). Already-cached refs are skipped.
func pullBlobsByRef(ctx context.Context, hub dssync.Hub, refs []string, paths config.Paths) (int, error) {
	missing := 0
	for _, ref := range refs {
		if ref == "" {
			continue
		}
		if _, err := readEnvBlob(paths, ref); err == nil {
			continue
		}
		reader, err := hub.GetBlob(ctx, blobHashHex(ref))
		if err != nil {
			missing++
			continue
		}
		ciphertext, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			return missing, fmt.Errorf("read blob %s: %w", ref, err)
		}
		// SEC-03: the blob_ref comes from a signed namespace event (or a
		// signature-authenticated snapshot), so the hub is an untrusted
		// bit-bucket. Recompute sha256 of the fetched ciphertext and reject on
		// mismatch so a malicious or buggy hub cannot substitute arbitrary bytes
		// under a valid content-addressed key. Do not cache a mismatched blob;
		// surface it as a missing/tampered blob.
		if err := verifyBlobContentHash(ref, ciphertext); err != nil {
			logging.Logger(ctx).Warn("blob content-address verification failed; not caching",
				"ref", ref, "err", err.Error())
			missing++
			continue
		}
		if err := writeEnvBlob(paths, ref, ciphertext); err != nil {
			return missing, fmt.Errorf("cache blob %s: %w", ref, err)
		}
	}
	return missing, nil
}

// verifyBlobContentHash asserts that ciphertext hashes to the sha256 embedded
// in the age_blob:<sha256> ref (SEC-03). The ref is sourced from a signed
// namespace event, so this turns content-addressing into a client-side
// integrity check the hub cannot bypass: a hub that returns wrong bytes under
// a valid key is detected as tampering.
func verifyBlobContentHash(ref string, ciphertext []byte) error {
	want := blobHashHex(ref)
	if want == "" {
		return fmt.Errorf("blob ref %s has no content hash", ref)
	}
	sum := sha256.Sum256(ciphertext)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Errorf("blob %s failed content-address verification: got %s (hub tampering?)", ref, got)
	}
	return nil
}

// blobRefFromEvent extracts an age_blob:<sha256> reference from an event
// payload, if the event type carries one (DRAFT-02).
func blobRefFromEvent(event state.Event) (string, bool) {
	switch event.Type {
	case dssync.EventDraftSnapshotCreated:
		var payload dssync.DraftSnapshotPayload
		if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err != nil {
			return "", false
		}
		if payload.BlobRef == "" {
			return "", false
		}
		return payload.BlobRef, true
	case dssync.EventEnvProfileUpdated:
		var payload dssync.EnvProfilePayload
		if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err != nil {
			return "", false
		}
		return payload.BlobRef, payload.BlobRef != ""
	default:
		return "", false
	}
}

func blobHashHex(ref string) string {
	hash, _ := envBlobHash(ref)
	return hash
}

// syncAckCacheKey keys the local_meta row caching the last-written ack's
// significant fields (consumed cursor + push watermark), so an unchanged cycle
// skips a redundant PUT.
func syncAckCacheKey(hubID string) string { return "sync_ack:" + hubID }

// maybeWriteSyncAck publishes this device's signed sync ack (P4-SYNC-06) after a
// FULLY-CLEAN cycle: no deferred push, no truncated/skipped/undecryptable pull,
// no quarantined/cursor-held apply, and no open durable skipped-event rows. Any
// of those means this device's view is incomplete, so it must not vouch for a
// watermark. The marker states the per-device transport cursor it has consumed,
// its push watermark, and its current HLC clock — the tombstone-safety clock a
// compactor mins over. Writing is best-effort: a PutAck failure logs a warning
// and never fails the sync (a missing ack only DELAYS a compactor's tombstone
// GC, never risks integrity).
func maybeWriteSyncAck(ctx context.Context, store *state.Store, hub dssync.Hub, hubID string, paths config.Paths, pull pullApplyOutcome, deferred bool) {
	log := logging.Logger(ctx)
	// Cleanliness gate — mirrors refuseIfIncompleteView's signals.
	if deferred || pull.stats.Quarantined > 0 || pull.stats.CursorHeld {
		return
	}
	if eh, ok := hub.(dssync.EncryptedHub); ok && eh.Stats != nil {
		if eh.Stats.Truncated > 0 || eh.Stats.Skipped > 0 || eh.Stats.Undecryptable > 0 {
			return
		}
	}
	skipped, err := store.OpenSkippedEvents(ctx)
	if err != nil {
		log.Warn("sync ack: read skipped events failed; not writing ack", "err", err.Error())
		return
	}
	if len(skipped) > 0 {
		return
	}
	cursor, err := store.HubDeviceCursors(ctx, hubID)
	if err != nil {
		log.Warn("sync ack: read cursors failed; not writing ack", "err", err.Error())
		return
	}
	push, err := store.PushSeqCursor(ctx, hubID)
	if err != nil {
		log.Warn("sync ack: read push cursor failed; not writing ack", "err", err.Error())
		return
	}
	// Skip a redundant PUT when neither the consumed cursor set nor the push
	// watermark changed since the last ack. We compare ONLY cursor+push, not the
	// HLC clock: the clock drifts every cycle (which would defeat the skip), and
	// an unchanged cursor+push means no event was consumed or produced, so the
	// previously published watermark still correctly bounds the consumed set.
	cacheVal := ackCacheValue(cursor, push)
	if cached, ok, cerr := store.GetLocalMeta(ctx, syncAckCacheKey(hubID)); cerr == nil && ok && cached == cacheVal {
		return
	}
	hlc, err := store.CurrentHLC(ctx)
	if err != nil {
		log.Warn("sync ack: read hlc failed; not writing ack", "err", err.Error())
		return
	}
	device, err := store.CurrentDevice(ctx)
	if err != nil {
		log.Warn("sync ack: read device failed; not writing ack", "err", err.Error())
		return
	}
	workspaceID, err := store.WorkspaceID(ctx)
	if err != nil {
		log.Warn("sync ack: read workspace failed; not writing ack", "err", err.Error())
		return
	}
	// P4-SYNC-05: the signed per-device head. Fold this device's own stream at
	// the push watermark so a pulling peer can check its received prefix against
	// our committed (seq, fold). Empty when nothing has been pushed, no fold seed
	// can be established, or the fold could not reach the push watermark
	// CONTIGUOUSLY (reached != push — e.g. a gap in this device's own stream from
	// a future backup/restore or pruning edge). Signing a fold that does not
	// correspond to `push` would make every honest peer raise a workspace-wide
	// false `fork`; omitting it degrades fail-safe to the unseeded/skip path the
	// verifier already handles. Mirrors the snapshot builder's
	// `seeded && reached == anchorSeq` guard.
	var foldedHash string
	if push > 0 {
		if reached, fh, seeded, ferr := store.DeviceFold(ctx, device.ID, push); ferr != nil {
			log.Warn("sync ack: fold local head failed; not writing ack", "err", ferr.Error())
			return
		} else if seeded && reached == push {
			foldedHash = fh
		}
	}
	marker := dssync.AckMarker{
		Cursor:           cursor,
		DeviceID:         device.ID,
		FoldedHash:       foldedHash,
		HLCWatermark:     hlc,
		ProducedAt:       hlc,
		PushedThroughSeq: push,
		WorkspaceID:      workspaceID,
	}
	keyStore, err := resolveKeyStore(ctx, paths, store)
	if err != nil {
		log.Warn("sync ack: resolve key store failed; not writing ack", "err", err.Error())
		return
	}
	signing, _, err := keyStore.EnsureSigning(ctx, device.ID, device.SigningPublicKey)
	if err != nil {
		log.Warn("sync ack: read signing identity failed; not writing ack", "err", err.Error())
		return
	}
	if err := dssync.SignAckMarker(&marker, signing.Private); err != nil {
		log.Warn("sync ack: sign failed; not writing ack", "err", err.Error())
		return
	}
	raw, err := json.Marshal(marker)
	if err != nil {
		log.Warn("sync ack: marshal failed; not writing ack", "err", err.Error())
		return
	}
	if err := hub.PutAck(ctx, device.ID, raw); err != nil {
		log.Warn("sync ack: put failed (best-effort; tombstone GC only delayed)", "err", err.Error())
		return
	}
	if err := store.SetLocalMeta(ctx, syncAckCacheKey(hubID), cacheVal); err != nil {
		log.Warn("sync ack: cache last-ack marker failed", "err", err.Error())
	}
}

// ackCacheValue is the canonical local_meta cache form of the fields whose
// change forces a new ack write: the consumed per-device cursor and the push
// watermark.
func ackCacheValue(cursor map[string]int64, push int64) string {
	raw, _ := json.Marshal(struct {
		Cursor map[string]int64 `json:"cursor"`
		Push   int64            `json:"push"`
	}{Cursor: cursor, Push: push})
	return string(raw)
}
