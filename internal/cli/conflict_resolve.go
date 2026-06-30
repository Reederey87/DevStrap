package cli

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
)

// enactConflictResolution makes a `conflicts resolve --keep-*` choice
// authoritative (P5-SYNC-04). Before this, the action was only written to
// resolution_json and the conflict.resolved event; nothing consumed it, so the
// project's actual remote was still whatever the deterministic HLC winner set
// and the user's choice was silently ignored at the data level.
//
// The choice is enacted by emitting fresh, dominating project.* events (which
// carry a new local HLC and therefore win LWW on every device) and applying the
// same mutation locally — reusing the proven apply machinery rather than a new
// convergence path. It returns a human-readable note describing what happened.
func enactConflictResolution(ctx context.Context, store *state.Store, opts *options, c state.Conflict, action string) (string, error) {
	switch c.Type {
	case dssync.ConflictSamePathDifferentRemote:
		return enactSamePathResolution(ctx, store, opts, c, action)
	case dssync.ConflictPendingDelete:
		return enactPendingDeleteResolution(ctx, store, opts, c, action)
	default:
		// rename_target_exists / untrustworthy_remote_time / event_hash_chain_break
		// / remote_conflict are advisory: there is no competing project variant to
		// pick, so closing the conflict (via conflict.resolved) is the whole action.
		return "", nil
	}
}

// enactSamePathResolution applies a keep-* choice to a same_path_different_remote
// conflict. After deterministic reconcile, every device shows the same winning
// remote at the path; the user picks which remote should own it going forward.
func enactSamePathResolution(ctx context.Context, store *state.Store, opts *options, c state.Conflict, action string) (string, error) {
	info, err := dssync.ParseSamePathConflictDetails(c.DetailsJSON)
	if err != nil {
		return "", err
	}
	current, err := store.ProjectByPath(ctx, info.Path)
	if err != nil {
		// The project was deleted out from under the conflict; nothing to enact.
		return "", nil
	}
	switch action {
	case "keep-local":
		// Re-assert the current (winning) remote with a fresh dominating event.
		// Same remote_key as the active row → no reconcile, no new conflict, and
		// any later re-delivery of the losing variant now loses on HLC. Preserve
		// the materialization state (omit LocalPath).
		chosen := dssync.ProjectPayload{
			Path:          info.Path,
			Type:          current.Type,
			RemoteURL:     current.RemoteURL,
			RemoteKey:     current.RemoteKey,
			DefaultBranch: current.DefaultBranch,
		}
		if err := upsertResolvedProject(ctx, store, opts, dssync.EventProjectUpdated, chosen, ""); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s pinned to %s", info.Path, chosen.RemoteKey), nil
	case "keep-remote":
		chosen, err := otherVariantPayload(ctx, store, info, current.RemoteKey)
		if err != nil {
			return "", err
		}
		// Switch the remote at the path with a SINGLE dominating project.updated
		// event (P5 review): a delete-then-add would tombstone the project on
		// every device and, if the add step failed, leave it deleted with no
		// re-add (data loss). One event is atomic. On peers this may transiently
		// re-open a same-path conflict that the same dominating event then wins
		// (the LWW model) — cosmetic, not data loss. Re-clone is required, so the
		// local path is passed to reset materialization to skeleton.
		localPath := filepath.Join(opts.paths().Root, filepath.FromSlash(info.Path))
		if err := upsertResolvedProject(ctx, store, opts, dssync.EventProjectUpdated, chosen, localPath); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s switched to %s (re-materialize to clone it)", info.Path, chosen.RemoteKey), nil
	case "keep-both":
		chosen, err := otherVariantPayload(ctx, store, info, current.RemoteKey)
		if err != nil {
			return "", err
		}
		sibling := siblingResolvedPath(info.Path, chosen.RemoteKey)
		chosen.Path = sibling
		localPath := filepath.Join(opts.paths().Root, filepath.FromSlash(sibling))
		if err := upsertResolvedProject(ctx, store, opts, dssync.EventProjectAdded, chosen, localPath); err != nil {
			return "", err
		}
		return fmt.Sprintf("kept both: %s stays on its remote; added %s -> %s", info.Path, sibling, chosen.RemoteKey), nil
	default:
		return "", nil
	}
}

// enactPendingDeleteResolution applies a keep-* choice to a pending_delete
// conflict (a remote delete held back because the local checkout was dirty).
func enactPendingDeleteResolution(ctx context.Context, store *state.Store, opts *options, c state.Conflict, action string) (string, error) {
	pathStr, err := dssync.ParsePendingDeleteConflictPath(c.DetailsJSON)
	if err != nil {
		return "", err
	}
	switch action {
	case "keep-local":
		// Keep the project: re-assert it with a fresh dominating event so the
		// pending delete loses LWW everywhere. Preserve materialization.
		current, err := store.ProjectByPath(ctx, pathStr)
		if err != nil {
			return "", nil
		}
		chosen := dssync.ProjectPayload{
			Path:          pathStr,
			Type:          current.Type,
			RemoteURL:     current.RemoteURL,
			RemoteKey:     current.RemoteKey,
			DefaultBranch: current.DefaultBranch,
		}
		if err := upsertResolvedProject(ctx, store, opts, dssync.EventProjectUpdated, chosen, ""); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s kept (delete discarded)", pathStr), nil
	case "keep-remote":
		// Honor the delete with a fresh dominating tombstone. The dirty working
		// tree on disk is left untouched for the user to clean up manually.
		if err := deleteResolvedProject(ctx, store, pathStr); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s deleted from the namespace (local files left on disk)", pathStr), nil
	default: // keep-both
		return "", fmt.Errorf("--keep-both does not apply to a delete conflict; use --keep-local or --keep-remote")
	}
}

// otherVariantPayload recovers the variant of a same_path conflict that is NOT
// the current local remote, by decoding the corresponding origin event.
func otherVariantPayload(ctx context.Context, store *state.Store, info dssync.SamePathConflictInfo, localKey string) (dssync.ProjectPayload, error) {
	otherEventID := info.LoserEventID
	if localKey == info.LoserKey {
		otherEventID = info.WinnerEventID
	}
	if otherEventID == "" {
		return dssync.ProjectPayload{}, fmt.Errorf("conflict %q has no recoverable alternate variant", info.Path)
	}
	ev, err := store.EventByID(ctx, otherEventID)
	if err != nil {
		return dssync.ProjectPayload{}, fmt.Errorf("recover alternate variant: %w", err)
	}
	p, err := dssync.ProjectPayloadFromEvent(ev.PayloadJSON)
	if err != nil {
		return dssync.ProjectPayload{}, err
	}
	if p.Type == "" {
		p.Type = "git_repo"
	}
	return p, nil
}

// upsertResolvedProject emits a fresh dominating project event and applies the
// mutation locally, mirroring addProject's mutate+emit pattern. A non-empty
// localPath resets device materialization state (used when a remote change
// requires a re-clone or when a new sibling is created); an empty localPath
// leaves the existing materialization untouched (used for in-place re-assert).
func upsertResolvedProject(ctx context.Context, store *state.Store, opts *options, eventType string, payload dssync.ProjectPayload, localPath string) error {
	if payload.Type == "" {
		payload.Type = "git_repo"
	}
	event, err := dssync.CreateProjectEvent(ctx, store, eventType, payload)
	if err != nil {
		return err
	}
	params := state.UpsertProjectParams{
		Path:                  payload.Path,
		Type:                  payload.Type,
		RemoteURL:             payload.RemoteURL,
		RemoteKey:             payload.RemoteKey,
		DefaultBranch:         payload.DefaultBranch,
		MaterializationPolicy: "lazy",
		SourceEventHLC:        event.HLC,
		SourceEventDeviceID:   event.DeviceID,
		SourceEventID:         event.ID,
	}
	if localPath != "" {
		params.LocalPath = localPath
		params.MaterializationState = "skeleton"
		params.DirtyState = "unknown"
	}
	if _, err := store.UpsertProject(ctx, params); err != nil {
		return err
	}
	if localPath != "" {
		if err := writeSkeleton(localPath, payload.Path, payload.RemoteURL); err != nil {
			return err
		}
	}
	return nil
}

// deleteResolvedProject emits a fresh dominating project.deleted event and
// tombstones the project locally so the deletion wins LWW everywhere.
func deleteResolvedProject(ctx context.Context, store *state.Store, nsPath string) error {
	event, err := dssync.CreateProjectEvent(ctx, store, dssync.EventProjectDeleted, dssync.ProjectPayload{Path: nsPath})
	if err != nil {
		return err
	}
	return store.WithTx(ctx, func(tx *state.Tx) error {
		return tx.TombstoneProject(ctx, nsPath, event.HLC)
	})
}

// siblingResolvedPath derives a deterministic sibling path for a keep-both
// resolution by suffixing the path with a slug of the alternate remote key, so
// both remotes coexist without colliding.
func siblingResolvedPath(nsPath, remoteKey string) string {
	slug := remoteKeySlug(remoteKey)
	if slug == "" {
		slug = "remote"
	}
	return nsPath + "." + slug
}

// remoteKeySlug turns host/org/repo into a filename-safe, stable slug.
func remoteKeySlug(remoteKey string) string {
	base := path.Base(remoteKey)
	base = strings.TrimSuffix(base, ".git")
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}
