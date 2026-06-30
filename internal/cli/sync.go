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
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Push and pull namespace events and materialize the tree",
		RunE: func(cmd *cobra.Command, args []string) error {
			if hubFile == "" {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("--hub-file is required until the production hub exists")}
			}
			return runSyncCycle(cmd.Context(), stdout, opts, hubFile, namespaceOnly, dryRun)
		},
	}
	cmd.Flags().StringVar(&hubFile, "hub-file", "", "file-backed test hub path")
	cmd.Flags().BoolVar(&namespaceOnly, "namespace-only", false, "sync namespace metadata only; skip materialization")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show sync plan without writing")
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
	hub := dssync.FileHub{Path: hubFile}
	hubID := "file:" + hubFile
	// SYNC-04: push cursor bounds the push side so a sync cycle re-uploads
	// only new local-origin events (HLC > push cursor), not the entire event
	// log including remote-origin events the hub already holds from their
	// origin device. The push cursor is a per-hub "push:<hubID>" row in
	// hub_cursors.
	pushCursor, err := store.HubCursor(ctx, "push:"+hubID)
	if err != nil {
		return err
	}
	localEvents, err := store.LocalPendingEvents(ctx, pushCursor)
	if err != nil {
		return err
	}
	if dryRun {
		_, err = fmt.Fprintf(stdout, "Would push %d local events to %s and pull namespace events\n", len(localEvents), hubFile)
		return err
	}
	// DRAFT-02: push local blobs referenced by pending events to the hub.
	if err := pushReferencedBlobs(ctx, hub, localEvents, opts.paths()); err != nil {
		return appError{code: exitNetwork, err: fmt.Errorf("push blobs: %w", err)}
	}
	if err := hub.Push(ctx, localEvents); err != nil {
		return appError{code: exitNetwork, err: err}
	}
	// SYNC-04: advance the push cursor to the highest pushed local HLC so the
	// next cycle only pushes newly-originated events.
	if len(localEvents) > 0 {
		var maxPushHLC int64
		for _, e := range localEvents {
			if e.HLC > maxPushHLC {
				maxPushHLC = e.HLC
			}
		}
		if err := store.AdvanceHubCursor(ctx, "push:"+hubID, maxPushHLC); err != nil {
			return err
		}
	}
	// EAGER-02: cursor-based incremental pull.
	cursor, err := store.HubCursor(ctx, hubID)
	if err != nil {
		return err
	}
	remoteEvents, err := hub.Pull(ctx, cursor)
	if err != nil {
		if errors.Is(err, dssync.ErrSnapshotRequired) {
			return appError{code: exitNetwork, err: err}
		}
		return err
	}
	// SYNC-01: ApplyEvents returns a low-water-mark safe cursor — the highest
	// HLC safe to advance to, never past a transiently-skipped (quarantined or
	// hash-chain-broken) event in this batch. Advancing past such an event
	// would permanently strand it, since Pull only returns HLC > cursor.
	safeCursor, err := dssync.ApplyEvents(ctx, store, remoteEvents)
	if err != nil {
		return err
	}
	if safeCursor > cursor {
		if err := store.AdvanceHubCursor(ctx, hubID, safeCursor); err != nil {
			return err
		}
	}
	// DRAFT-02: pull referenced blobs from the hub and cache them locally.
	missingBlobs, err := pullReferencedBlobs(ctx, hub, remoteEvents, opts.paths())
	if err != nil {
		return appError{code: exitNetwork, err: fmt.Errorf("pull blobs: %w", err)}
	}
	if missingBlobs > 0 {
		_, _ = fmt.Fprintf(stdout, "warning: %d referenced blob(s) missing from hub; materialization may be incomplete\n", missingBlobs)
	}
	if namespaceOnly {
		_, err = fmt.Fprintf(stdout, "Synced namespace events: pushed %d, pulled %d\n", len(localEvents), len(remoteEvents))
		return err
	}
	// EAGER-01: eager materialization.
	projects, err := store.SkeletonProjects(ctx)
	if err != nil {
		return err
	}
	results := materializePass(ctx, store, opts, projects)
	// HUB-05: reclaim locally-cached blobs no longer referenced.
	if removed, gcErr := gcUnreferencedBlobs(ctx, store, opts.paths()); gcErr == nil && removed > 0 {
		_, _ = fmt.Fprintf(stdout, "GC'd %d unreferenced blob(s)\n", removed)
	}
	_, err = fmt.Fprintf(stdout, "Synced events: pushed %d, pulled %d; materialized %d/%d projects\n",
		len(localEvents), len(remoteEvents), results.succeeded, results.total)
	return err
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
	missing := 0
	for _, event := range events {
		ref, ok := blobRefFromEvent(event)
		if !ok {
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
		// SEC-03: the blob_ref comes from a signed namespace event, so the hub
		// is an untrusted bit-bucket. Recompute sha256 of the fetched
		// ciphertext and reject on mismatch so a malicious or buggy hub cannot
		// substitute arbitrary bytes under a valid content-addressed key. Do
		// not cache a mismatched blob; surface it as a missing/tampered blob.
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
