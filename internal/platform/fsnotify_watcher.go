package platform

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

type NativeWatcher struct {
	Debounce   time.Duration
	MaxLatency time.Duration
}

func (w NativeWatcher) Name() string { return "fsnotify" }

func (w NativeWatcher) Watch(ctx context.Context, root string, events chan<- FSEvent) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create fsnotify watcher: %w", err)
	}
	defer func() { _ = watcher.Close() }()

	if err := addRecursiveWatch(watcher, root); err != nil {
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
			if event.Has(fsnotify.Create) {
				if info, statErr := os.Stat(event.Name); statErr == nil && info.IsDir() {
					if addErr := addRecursiveWatch(watcher, event.Name); addErr != nil {
						return addErr
					}
				}
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

func addRecursiveWatch(watcher *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			return nil
		}
		if shouldSkipWatchDir(entry.Name()) && path != root {
			return filepath.SkipDir
		}
		if err := watcher.Add(path); err != nil {
			return fmt.Errorf("watch %s: %w", path, err)
		}
		return nil
	})
}

func shouldSkipWatchDir(name string) bool {
	switch name {
	case ".git", "node_modules", ".devstrap", "vendor":
		return true
	default:
		return false
	}
}
