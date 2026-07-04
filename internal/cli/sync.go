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
)

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
			return runSyncCycle(cmd.Context(), stdout, opts, hubFile, namespaceOnly, dryRun)
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
func runSyncCycle(ctx context.Context, stdout io.Writer, opts *options, hubFile string, namespaceOnly, dryRun bool) error {
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
	if dryRun {
		// P6-CLI-05: print the resolved hub ID (file:<path> / r2:<ws…>), never
		// the raw --hub-file flag, which is empty when the hub comes from config.
		_, err = fmt.Fprintf(stdout, "Would push %d local events to %s and pull namespace events\n", len(localEvents), hubID)
		return err
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
		_, _ = fmt.Fprintln(stdout, "Recovering from hub snapshot (retention floor passed our cursor)…")
		imported, rerr := recoverFromSnapshot(ctx, stdout, store, hub, hubID, opts.paths(), buildKeyring(ctx, opts, store))
		if rerr != nil {
			return rerr
		}
		if !imported {
			// Keyless joiner: awaiting a grant. recoverFromSnapshot already
			// printed the defer message; nothing to materialize or push yet.
			return nil
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
		_, _ = fmt.Fprintf(stdout, "warning: %d referenced blob(s) missing from hub; materialization may be incomplete\n", missingBlobs)
	}

	// P4-SEC-07: age-triggered periodic WCK rotation. Deliberately AFTER the
	// pull — a freshly ingested grant resets the local epoch age, which
	// suppresses fleet-wide rotation storms (whichever device syncs first past
	// the deadline rotates; everyone else pulls the new epoch and stands down)
	// — and BEFORE the push, so the mint's grant events ride THIS cycle.
	if rotated, rerr := maybeRotateWorkspaceKey(ctx, stdout, opts, store); rerr != nil {
		return rerr
	} else if rotated {
		// The localEvents snapshot above predates the mint; re-read so the
		// just-minted grant events are pushed in this same cycle.
		localEvents, err = store.LocalPendingEventsBySeq(ctx, pushCursor)
		if err != nil {
			return err
		}
	}

	// P6-SEC-02 founder/join gate. After the pull (which ingested any grant for
	// this device), decide whether this device holds a workspace key. If not,
	// either found the workspace (only when the hub is genuinely empty and this
	// device did not `init --join`) or defer the push until a grant arrives.
	rawSeen := len(remoteEvents)
	if eh, ok := hub.(dssync.EncryptedHub); ok && eh.Stats != nil {
		rawSeen = eh.Stats.RawSeen
	}
	pushed, deferred, ferr := pushLocalEventsGated(ctx, stdout, opts, store, hub, hubID, localEvents, rawSeen)
	if ferr != nil {
		return ferr
	}

	if namespaceOnly {
		if deferred {
			_, err = fmt.Fprintf(stdout, "Synced namespace events: pulled %d; %d local event(s) queued awaiting workspace key grant\n", len(remoteEvents), len(localEvents))
			return err
		}
		_, err = fmt.Fprintf(stdout, "Synced namespace events: pushed %d, pulled %d\n", pushed, len(remoteEvents))
		return err
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
		_, _ = fmt.Fprintf(stdout, "GC'd %d unreferenced blob(s)\n", removed)
	}
	_, err = fmt.Fprintf(stdout, "Synced events: pushed %d, pulled %d; materialized %d/%d projects (%d skipped)\n",
		pushed, len(remoteEvents), results.succeeded, results.total, results.skipped)
	return err
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
	return pullApplyOutcome{events: remoteEvents, stats: stats}, nil
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
func maybeRotateWorkspaceKey(ctx context.Context, stdout io.Writer, opts *options, store *state.Store) (bool, error) {
	maxAge := keyRotateMaxAge(opts)
	if maxAge <= 0 {
		return false, nil
	}
	epoch, created, err := store.ActiveKeyEpochAge(ctx)
	if err != nil {
		return false, err
	}
	if epoch == 0 || time.Since(created) < maxAge {
		return false, nil
	}
	kr := buildKeyring(ctx, opts, store)
	newEpoch, grants, rerr := kr.Rotate(ctx)
	if rerr != nil {
		// FATAL for the cycle (post-#56 Codex review, P1): Rotate wraps every
		// grant before writing any state, so a failure here is either
		// harmless-and-early (nothing recorded; a malformed recipient row
		// needs fixing, not retrying) or a rare DB/custody fault mid-commit —
		// and in the latter case pushing would seal this cycle's events under
		// a half-minted epoch whose grants never published, while the fresh
		// created_at suppresses the retry. Aborting keeps the cycle's events
		// queued and the failure loud.
		return false, fmt.Errorf("periodic workspace key rotation failed (fix the cause or disable via keys.rotate_max_age=0 / --key-max-age 0): %w", rerr)
	}
	_, _ = fmt.Fprintf(stdout, "Rotated workspace key to epoch %d (epoch %d exceeded keys.rotate_max_age %s); %d grant event(s) ride this push\n",
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
func pushLocalEventsGated(ctx context.Context, stdout io.Writer, opts *options, store *state.Store, hub dssync.Hub, hubID string, localEvents []state.Event, rawSeen int) (pushed int, deferred bool, err error) {
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
			_, _ = fmt.Fprintf(stdout, "Awaiting workspace key grant: %d local event(s) queued. "+
				"On an approved device run 'devstrap devices enroll … --approve' (or 'devices approve <id>'), then re-run sync.\n", len(localEvents))
			return 0, true, nil
		}
	}

	// DRAFT-02: push local blobs referenced by pending events to the hub.
	if err := pushReferencedBlobs(ctx, hub, localEvents, opts.paths()); err != nil {
		return 0, false, appError{code: exitNetwork, err: fmt.Errorf("push blobs: %w", err)}
	}
	if err := hub.Push(ctx, localEvents); err != nil {
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
		_, _ = fmt.Fprintf(stdout, "Removed %d superseded blob(s) from the hub\n", drained)
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
func pushReferencedBlobs(ctx context.Context, hub dssync.Hub, events []state.Event, paths config.Paths) error {
	for _, event := range events {
		ref, ok := blobRefFromEvent(event)
		if !ok {
			continue
		}
		cached, err := readEnvBlob(paths, ref)
		if err != nil {
			return fmt.Errorf("push blob %s: cannot read local cache: %w", ref, err)
		}
		if err := hub.PutBlob(ctx, blobHashHex(ref), bytes.NewReader(cached)); err != nil {
			return fmt.Errorf("push blob %s: %w", ref, err)
		}
	}
	return nil
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
	if event.Type != dssync.EventDraftSnapshotCreated {
		return "", false
	}
	var payload dssync.DraftSnapshotPayload
	if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err != nil {
		return "", false
	}
	if payload.BlobRef == "" {
		return "", false
	}
	return payload.BlobRef, true
}

func blobHashHex(ref string) string {
	hash, _ := envBlobHash(ref)
	return hash
}
