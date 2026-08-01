package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Reederey87/DevStrap/internal/ignore"
	"github.com/Reederey87/DevStrap/internal/state"
	"golang.org/x/text/unicode/norm"
)

const stagingOrphanMinAge = time.Hour
const stagingSweepLastSuccessKey = "staging_sweep_last_success"

type stagingSweepAction struct {
	Path    string
	Removed bool
	Reason  string
}

func stagingProjectName(name string) (string, bool) {
	if !ignore.IsStagingDirName(name) {
		return "", false
	}
	i := strings.LastIndex(name, ignore.StagingDirMarker)
	project := strings.TrimPrefix(name[:i], ".")
	return project, project != ""
}

func sweepStagingOrphans(ctx context.Context, store *state.Store, opts *options, now time.Time) ([]stagingSweepAction, error) {
	projects, err := store.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	registered := make(map[string]state.ProjectStatus, len(projects))
	registeredPaths := make([]string, 0, len(projects))
	for _, p := range projects {
		local := p.LocalPath
		if local == "" {
			local = filepath.Join(opts.paths().Root, filepath.FromSlash(p.Path))
		}
		// NFC on both sides. Registered paths come from NFC-normalized
		// namespace rows, while a walk path carries the on-disk parent
		// spelling, which on APFS/HFS+ can be NFD. An exact-string miss would
		// silently downgrade a MAPPED candidate to the unmapped branch, whose
		// only protection is the age window.
		key := norm.NFC.String(filepath.Clean(local))
		registered[key] = p
		registeredPaths = append(registeredPaths, key)
	}
	var actions []stagingSweepAction
	root := opts.paths().Root
	osRoot, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("open managed root: %w", err)
	}
	defer func() { _ = osRoot.Close() }()
	matcher := ignore.DefaultMatcher()
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			// One unreadable directory must not abort the sweep. Aborting also
			// means the success marker is never recorded, so the sweep re-runs
			// every interval and never reaches candidates beyond the error.
			if errors.Is(walkErr, fs.ErrPermission) || errors.Is(walkErr, fs.ErrNotExist) {
				if entry != nil && entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			return walkErr
		}
		if path == root {
			return nil
		}
		cleanPath := norm.NFC.String(filepath.Clean(path))
		if !ignore.IsStagingDirName(entry.Name()) {
			// Prune generated directories exactly as the scanner does. Without
			// this the sweep descends into every node_modules/.git/.venv in the
			// managed tree on every interval — the measured DevStrap namespace
			// is ~7x larger unpruned (spec/05) — and it could match a directory
			// that merely LOOKS like staging inside a dependency and delete it.
			// A real staging dir is always a SIBLING of a project target, so
			// nothing reachable only through a pruned directory is a candidate.
			//
			// Order matters: the staging check runs FIRST, because W12-01 added
			// the staging pattern to the default prune set, so ShouldPruneDir
			// returns true for the very directories this sweep exists to find.
			if entry.IsDir() {
				rel, relErr := filepath.Rel(root, path)
				if relErr == nil && matcher.ShouldPruneDir(entry.Name(), filepath.ToSlash(rel)) {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if _, ok := registered[cleanPath]; ok {
			actions = append(actions, stagingSweepAction{Path: path, Reason: "not deleted: path matches a registered project row"})
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// An exact match is not enough. A LEGACY row registered UNDERNEATH a
		// staging-shaped ancestor (accepted before the reserved-basename guard
		// existed) would otherwise be deleted wholesale along with the ancestor
		// directory. New rows can no longer take this shape — pathkey.Clean
		// rejects the component at the store layer — but legacy rows are
		// precisely what a guard is for.
		if under, ok := registeredUnder(cleanPath, registeredPaths); ok {
			actions = append(actions, stagingSweepAction{Path: path, Reason: "not deleted: ancestor of registered project " + under})
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		name, mapped := stagingProjectName(entry.Name())
		var unlock func()
		if mapped {
			project, ok := registered[norm.NFC.String(filepath.Clean(filepath.Join(filepath.Dir(path), name)))]
			if !ok {
				mapped = false
			} else {
				unlock, err = acquireRepoLock(opts.paths().Home, project.ID)
				if err != nil {
					var ae appError
					if errors.As(err, &ae) && ae.code == exitConflict {
						actions = append(actions, stagingSweepAction{Path: path, Reason: "not deleted: project repo lock is held"})
						if entry.IsDir() {
							return filepath.SkipDir
						}
						return nil
					}
					return err
				}
				defer unlock()
			}
		}
		info, err := os.Lstat(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			actions = append(actions, stagingSweepAction{Path: path, Reason: "not deleted: candidate is not a real directory"})
			return nil
		}
		if !mapped && now.Sub(info.ModTime()) < stagingOrphanMinAge {
			actions = append(actions, stagingSweepAction{Path: path, Reason: "not deleted: unmapped candidate is younger than 1h"})
			return filepath.SkipDir
		}
		// G122 / symlink TOCTOU: Lstat above and the removal below are two
		// separate syscalls, so a directory can be swapped for a symlink in
		// between — on a path that DELETES, that is the whole ballgame. The
		// removal is therefore root-scoped: os.Root refuses to traverse a
		// symlink out of the managed tree, so the worst a winning racer
		// achieves is an error instead of a deletion outside ~/Code.
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return fmt.Errorf("resolve staging orphan %s: %w", path, relErr)
		}
		if err := osRoot.RemoveAll(rel); err != nil {
			return fmt.Errorf("remove staging orphan %s: %w", path, err)
		}
		actions = append(actions, stagingSweepAction{Path: path, Removed: true, Reason: "removed: orphan clone-staging directory"})
		return filepath.SkipDir
	})
	return actions, err
}

func maybeSweepStagingOrphansAfterSync(ctx context.Context, stderr io.Writer, opts *options, store *state.Store, now time.Time) (int, error) {
	interval, err := wipGCInterval(opts)
	if err != nil {
		return 0, err
	}
	if interval == 0 {
		return 0, nil
	}
	due, err := maintenanceDue(ctx, store, stagingSweepLastSuccessKey, interval, now)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: staging-orphan sweep scheduling failed; sweep will retry: %s\n", scrubbed(err))
		return 0, nil
	}
	if !due {
		return 0, nil
	}
	actions, err := sweepStagingOrphans(ctx, store, opts, now)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: staging-orphan sweep failed; sweep will retry: %s\n", scrubbed(err))
		return 0, nil
	}
	removed := 0
	for _, a := range actions {
		if a.Removed {
			removed++
		}
		_, _ = fmt.Fprintf(stderr, "staging-orphan sweep: %s: %s\n", a.Path, a.Reason)
	}
	raw, err := json.Marshal(now.UTC())
	if err != nil {
		return removed, err
	}
	if err := store.SetLocalMeta(ctx, stagingSweepLastSuccessKey, string(raw)); err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: staging-orphan sweep could not record success; sweep will retry: %s\n", scrubbed(err))
	}
	return removed, nil
}

// registeredUnder reports whether candidate is a strict path-ancestor of any
// registered project path, returning the first such project path. Deleting an
// ancestor removes everything beneath it, so the candidate must be spared.
func registeredUnder(candidate string, registeredPaths []string) (string, bool) {
	prefix := candidate + string(os.PathSeparator)
	for _, p := range registeredPaths {
		if strings.HasPrefix(p, prefix) {
			return p, true
		}
	}
	return "", false
}
