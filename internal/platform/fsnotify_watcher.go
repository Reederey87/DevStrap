package platform

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Reederey87/DevStrap/internal/ignore"
	"github.com/fsnotify/fsnotify"
)

type NativeWatcher struct {
	Debounce   time.Duration
	MaxLatency time.Duration
	mu         sync.Mutex
	live       map[*fsnotify.Watcher]struct{}
}

func (w *NativeWatcher) Name() string { return "fsnotify" }

func (w *NativeWatcher) Watch(ctx context.Context, root string, events chan<- FSEvent) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create fsnotify watcher: %w", err)
	}
	defer func() { _ = watcher.Close() }()

	w.mu.Lock()
	if w.live == nil {
		w.live = make(map[*fsnotify.Watcher]struct{})
	}
	w.live[watcher] = struct{}{}
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.live, watcher)
		w.mu.Unlock()
	}()

	matcher, err := ignore.CompileFromDir(root, true)
	if err != nil {
		matcher = ignore.DefaultMatcher()
	}
	return w.watch(ctx, root, events, watcher, matcher)
}

func (w *NativeWatcher) watch(
	ctx context.Context,
	root string,
	events chan<- FSEvent,
	watcher *fsnotify.Watcher,
	matcher *ignore.Matcher,
) error {
	if err := addRecursiveWatch(watcher, root, root, matcher); err != nil {
		return err
	}

	debounce := w.Debounce
	if debounce <= 0 {
		debounce = 250 * time.Millisecond
	}
	maxLatency := w.MaxLatency
	if maxLatency <= 0 {
		maxLatency = 2 * time.Second
	}

	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	var timerC <-chan time.Time
	var firstPending time.Time
	pending := false

	arm := func(delay time.Duration) {
		if timerC != nil && !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(delay)
		timerC = timer.C
	}
	disarm := func() {
		if timerC != nil && !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timerC = nil
	}
	flush := func(at time.Time) error {
		if !pending {
			return nil
		}
		pending = false
		disarm()
		select {
		case events <- FSEvent{Kind: FSEventScan, Path: root, At: at}:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			return fmt.Errorf("fsnotify watcher: %w", err)
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			// Ignore events whose path is not a real location inside this watch
			// root. The kqueue backend emits events with an EMPTY or "." name
			// after a watch is removed, and acting on those is actively
			// dangerous: `.` resolves against the process working directory, so
			// the create branch below would recursively watch the daemon's cwd
			// instead of anything in the workspace.
			if !eventPathUnderRoot(root, event.Name) {
				continue
			}

			// Chmod cannot change the namespace. Exact equality, not Has: a
			// Write|Chmod still counts as a change.
			if event.Op == fsnotify.Chmod {
				continue
			}

			// Rename and delete can make fsnotify drop the underlying watch
			// before this event is delivered. Remove is therefore best-effort:
			// either outcome means DevStrap no longer retains the stale path.
			//
			// This runs BEFORE any decision to ignore the event. Suppressing a
			// junk-named path's HINT must never also skip its descriptor
			// bookkeeping: a directory named `backup~` gets watched (the
			// canonical policy has no opinion on it, correctly), so short-
			// circuiting here would leak every watch beneath it when it left the
			// tree — reintroducing the exact leak this change exists to close.
			if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
				removeWatchTree(watcher, event.Name)
			}

			info, statErr := os.Stat(event.Name)
			isDir := statErr == nil && info.IsDir()
			ignored := isWatcherJunk(event.Name) || shouldSkipWatchPath(matcher, root, event.Name, isDir)
			if statErr != nil {
				// The path is gone, so "was it a directory?" is unanswerable, and
				// the two answers disagree: `dist` is pruned as a directory but
				// is ordinary content as a file. Ignore only when BOTH readings
				// agree, so deleting a FILE named `build` still reports — the
				// previous forced-directory reading swallowed those deletions
				// silently, while reporting writes to the same file.
				ignored = isWatcherJunk(event.Name) ||
					(shouldSkipWatchPath(matcher, root, event.Name, true) &&
						shouldSkipWatchPath(matcher, root, event.Name, false))
			}

			// A directory created or renamed into place after the initial walk
			// must join the watch set only when its root-relative path survives
			// the same compiled policy used by the initial walk.
			if (event.Has(fsnotify.Create) || event.Has(fsnotify.Rename)) && isDir && !ignored {
				if addErr := addRecursiveWatch(watcher, event.Name, root, matcher); addErr != nil {
					return addErr
				}
			}
			if ignored {
				continue
			}
			now := time.Now()
			if !pending {
				pending = true
				firstPending = now
			}
			if now.Sub(firstPending) >= maxLatency {
				if err := flush(now); err != nil {
					return err
				}
				continue
			}
			arm(debounce)
		case at := <-timerC:
			if err := flush(at); err != nil {
				return err
			}
		}
	}
}

// WatchedDirs reports the directories currently registered across every live
// Watch call on this instance.
func (w *NativeWatcher) WatchedDirs() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	total := 0
	for watcher := range w.live {
		total += len(watcher.WatchList())
	}
	return total
}

// eventPathUnderRoot reports whether an event path names a location at or below
// root.
//
// This is a guard against the backend, not against the filesystem. fsnotify's
// kqueue backend emits events with an empty or "." Name after a watch is
// removed — observed directly while testing descriptor release on darwin — and a
// relative name silently resolves against the process working directory. Without
// this check a removed watch could make the adapter walk and watch the daemon's
// cwd, so the guard is load-bearing rather than defensive tidiness.
func eventPathUnderRoot(root, name string) bool {
	if name == "" {
		return false
	}
	rel, err := filepath.Rel(root, name)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// addRecursiveWatch registers watches for root and every directory below it that
// survives the compiled policy.
//
// A directory that VANISHES mid-walk is not an error. This is the difference
// between a watcher that survives a working machine and one that does not: any
// `go test`, `npm`, or build run inside a watched project creates and deletes
// temporary subdirectories, so a walk that fails on the resulting ENOENT kills
// the watch plane within seconds of arming — and it re-dies on every retry, so
// the native watcher is effectively never alive and the whole point of pruning
// through the compiler is unobservable. Losing a watch on a directory that no
// longer exists costs nothing; the periodic cycle is the backstop either way.
func addRecursiveWatch(watcher *fsnotify.Watcher, root, watchRoot string, matcher *ignore.Matcher) error {
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			// The entry disappeared between being listed and being visited.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			// An unreadable directory is a local, recoverable condition — a test
			// fixture exercising EACCES, a root-owned container bind-mount — not
			// a reason to abandon the tree. Before P9-DAEMON-01 this returned a
			// hard error that propagated out of Watch into watchAll, whose shared
			// cancel then tore down every OTHER, healthy root's watch; the
			// re-discovery loop re-walked the whole tree each interval and failed
			// on the same directory forever. Skip the subtree instead: the
			// periodic cycle remains the backstop for anything under it, exactly
			// as it is for a directory that vanished.
			if errors.Is(err, fs.ErrPermission) {
				if entry != nil && entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			return err
		}
		if !entry.IsDir() {
			return nil
		}
		if path != watchRoot && shouldSkipWatchPath(matcher, watchRoot, path, true) {
			return filepath.SkipDir
		}
		if addErr := watcher.Add(path); addErr != nil {
			if errors.Is(addErr, fs.ErrNotExist) {
				return nil
			}
			// Same reasoning as the walk-callback case above (P9-DAEMON-01): an
			// unreadable directory is local and recoverable, and the kernel
			// refuses the watch registration itself even when the entry LISTS
			// fine from a readable parent — so this branch, not just the walk
			// error, is what a `chmod 0000` subtree actually trips. Skip it and
			// keep watching the readable siblings.
			if errors.Is(addErr, fs.ErrPermission) {
				return fs.SkipDir
			}
			return fmt.Errorf("watch %s: %w", path, addErr)
		}
		return nil
	})
	// The walk root itself can be gone by the time we get here — a directory
	// created and removed before its create event was handled.
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// watcherOnlyPrunes are directories the WATCHER skips that the canonical
// ignore compiler deliberately does not list.
//
// The distinction is the point. The compiler's defaults answer "is this
// project content?" — they drive `internal/scan` adoption and which files ride
// a draft bundle, so adding to them changes what gets SYNCED. These two answer
// only "is it worth a watch descriptor?", which is a budget question local to
// this adapter:
//
//   - vendor/ is committed source in many Go projects. Pruning it from the
//     canonical set would silently drop it from a non-git draft folder's
//     bundle; pruning it from the watcher only costs us hints for files that
//     change when dependencies are re-vendored, which the periodic cycle
//     catches anyway.
//   - .devstrap/ is DevStrap's own state home. The compiler prunes its tmp/
//     and cache/ subtrees at any depth (P6-XP-02) and deliberately keeps the
//     rest visible; the watcher has no reason to watch any of it.
//   - .Trash/ is the OS's, not the project's. Nothing inside it is a namespace
//     change, but it is a real directory a user can create anywhere.
var watcherOnlyPrunes = map[string]struct{}{
	"vendor":    {},
	".devstrap": {},
	".Trash":    {},
}

// isWatcherJunk reports whether a path's own name marks it as OS or editor
// scratch that can never represent a namespace change (PLAT-04).
//
// It is checked UNCONDITIONALLY rather than through the compiled matcher, and
// that is the whole point of it existing separately. Two reasons:
//
//  1. The matcher applies the user's .devstrapignore, where a negation (`!*~`)
//     would switch adapter-level noise filtering back ON. A user asking for
//     `foo~` to be SYNCED is not asking to be woken up every time vim writes a
//     backup, so content policy is the wrong lever for this.
//  2. `*~` must not be content policy at all. It is a glob over user filenames,
//     not a fixed name like .DS_Store, so putting it in the canonical defaults
//     would silently stop syncing a draft legitimately named `proposal~` — the
//     same class of mistake as pruning vendor/ there.
//
// Dropping a hint is safe by construction: a hint is an optimization, so the
// worst case is one interval of extra latency before periodic convergence sees
// the change. The file itself still syncs.
func isWatcherJunk(path string) bool {
	base := filepath.Base(path)
	// Canonical OS junk comes from internal/ignore's single table rather than a
	// copy here. PLAT-01 is precisely the finding that these lists must not
	// diverge, and an earlier draft of this function had already drifted from it
	// (it omitted ehthumbs.db).
	if ignore.IsOSJunkName(base) {
		return true
	}
	// Watcher-local editor scratch: vim's write probe, vim/gedit backups (foo~),
	// emacs autosaves (#foo#) and lock files (.#foo). These stay local because
	// they are glob-shaped over user filenames — see the docstring above.
	if base == "4913" {
		return true
	}
	return strings.HasSuffix(base, "~") ||
		(strings.HasPrefix(base, "#") && strings.HasSuffix(base, "#")) ||
		strings.HasPrefix(base, ".#")
}

func shouldSkipWatchPath(matcher *ignore.Matcher, root, path string, isDir bool) bool {
	if matcher == nil {
		matcher = ignore.DefaultMatcher()
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		// Fail open: this plane emits hints, so the cost of a stray hint is one
		// redundant namespace-only cycle, while the cost of pruning a path we
		// cannot place is silence about real changes.
		return false
	}
	relSlash := filepath.ToSlash(rel)
	if isDir {
		base := filepath.Base(path)
		if _, ok := watcherOnlyPrunes[base]; ok {
			return true
		}
		// A junk-named DIRECTORY is not worth a descriptor either, and pruning it
		// at registration is what keeps the event-side junk check from having to
		// choose between suppressing a hint and doing its bookkeeping.
		if isWatcherJunk(path) {
			return true
		}
		return matcher.ShouldPruneDir(base, relSlash)
	}
	return matcher.Match(relSlash, false)
}

func removeWatchTree(watcher *fsnotify.Watcher, root string) {
	prefix := root + string(filepath.Separator)
	for _, watched := range watcher.WatchList() {
		if watched == root || len(watched) > len(prefix) && watched[:len(prefix)] == prefix {
			_ = watcher.Remove(watched)
		}
	}
}
