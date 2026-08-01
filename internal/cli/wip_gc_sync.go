package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Reederey87/DevStrap/internal/redact"
	"github.com/Reederey87/DevStrap/internal/state"
)

const (
	defaultWipGCInterval = 24 * time.Hour
	wipGCIntervalKey     = "wip.gc_interval"
	wipTTLConfigKey      = "wip.ttl"
	wipGCLastSuccessKey  = "wip_gc_last_success"
)

func wipGCInterval(opts *options) (time.Duration, error) {
	return parseWipDuration(opts, wipGCIntervalKey, defaultWipGCInterval)
}

func wipTTL(opts *options) (time.Duration, error) {
	return parseWipDuration(opts, wipTTLConfigKey, defaultWipGCTTL)
}

func parseWipDuration(opts *options, key string, defaultValue time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(opts.v.GetString(key))
	if raw == "" {
		return defaultValue, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, appError{code: exitInvalidConfig, err: fmt.Errorf("invalid %s %q: %w", key, raw, err)}
	}
	if d < 0 {
		return 0, appError{code: exitInvalidConfig, err: fmt.Errorf("invalid %s %q: must be >= 0 (0 disables WIP GC)", key, raw)}
	}
	return d, nil
}

func wipGCDue(ctx context.Context, store *state.Store, interval time.Duration, now time.Time) (bool, error) {
	return maintenanceDue(ctx, store, wipGCLastSuccessKey, interval, now)
}

func maintenanceDue(ctx context.Context, store *state.Store, key string, interval time.Duration, now time.Time) (bool, error) {
	raw, ok, err := store.GetLocalMeta(ctx, key)
	if err != nil {
		return false, err
	}
	if !ok {
		return true, nil
	}
	var last time.Time
	if err := json.Unmarshal([]byte(raw), &last); err != nil || last.IsZero() {
		// Advisory scheduling state must never permanently wedge hygiene.
		return true, nil
	}
	age := now.Sub(last)
	return age < 0 || age >= interval, nil
}

func maybeGCWipRefsAfterSync(ctx context.Context, stderr io.Writer, opts *options, store *state.Store, now time.Time) (int, error) {
	interval, err := wipGCInterval(opts)
	if err != nil {
		return 0, err
	}
	ttl, err := wipTTL(opts)
	if err != nil {
		return 0, err
	}
	if interval == 0 || ttl == 0 {
		return 0, nil
	}
	due, err := wipGCDue(ctx, store, interval, now)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: automatic WIP GC scheduling failed; sweep will retry: %s\n", scrubbed(err))
		return 0, nil
	}
	if !due {
		return 0, nil
	}
	result, failedDeletes, err := sweepWipRefs(ctx, store, opts, wipGCOptions{TTL: ttl})
	if err != nil {
		var configErr appError
		if errors.As(err, &configErr) && configErr.code == exitInvalidConfig {
			return 0, err
		}
		_, _ = fmt.Fprintf(stderr, "warning: automatic WIP GC failed after sync; sweep will retry: %s\n", scrubbed(err))
		return 0, nil
	}
	for _, warning := range result.Warnings {
		message := redact.Scrub(warning.Message)
		_, _ = fmt.Fprintf(stderr, "warning: automatic WIP GC for %s: %s\n", warning.Path, message)
		if p, projectErr := store.ProjectByPath(ctx, warning.Path); projectErr == nil {
			_ = store.RecordProjectWarning(ctx, p.ID, "wip gc: "+message)
		}
	}
	if failedDeletes > 0 {
		_, _ = fmt.Fprintf(stderr, "warning: automatic WIP GC could not delete %d nominated ref(s); sweep will retry\n", failedDeletes)
		return countWipGCDeletes(result), nil
	}
	raw, err := json.Marshal(now.UTC())
	if err != nil {
		return 0, err
	}
	if err := store.SetLocalMeta(ctx, wipGCLastSuccessKey, string(raw)); err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: automatic WIP GC could not record success; sweep will retry: %s\n", scrubbed(err))
		return countWipGCDeletes(result), nil
	}
	return countWipGCDeletes(result), nil
}

func countWipGCDeletes(result wipGCResult) int {
	n := 0
	for _, action := range result.Actions {
		if action.Delete {
			n++
		}
	}
	return n
}
