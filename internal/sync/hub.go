package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/Reederey87/DevStrap/internal/state"
)

var ErrSnapshotRequired = errors.New("full snapshot required")

type FileHub struct {
	Path         string
	RetentionHLC int64
}

func (h FileHub) Push(ctx context.Context, events []state.Event) error {
	all, err := h.read()
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, event := range all {
		seen[event.ID] = true
	}
	for _, event := range events {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !seen[event.ID] {
			all = append(all, event)
			seen[event.ID] = true
		}
	}
	sortEvents(all)
	return h.write(all)
}

func (h FileHub) Pull(ctx context.Context, afterHLC int64) ([]state.Event, error) {
	if h.RetentionHLC > 0 && afterHLC < h.RetentionHLC {
		return nil, ErrSnapshotRequired
	}
	all, err := h.read()
	if err != nil {
		return nil, err
	}
	var out []state.Event
	for _, event := range all {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if event.HLC > afterHLC {
			out = append(out, event)
		}
	}
	sortEvents(out)
	return out, nil
}

func (h FileHub) read() ([]state.Event, error) {
	raw, err := os.ReadFile(h.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read hub: %w", err)
	}
	var events []state.Event
	if len(raw) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &events); err != nil {
		return nil, fmt.Errorf("decode hub: %w", err)
	}
	return events, nil
}

func (h FileHub) write(events []state.Event) error {
	if err := os.MkdirAll(filepath.Dir(h.Path), 0o700); err != nil {
		return fmt.Errorf("create hub dir: %w", err)
	}
	raw, err := json.MarshalIndent(events, "", "  ")
	if err != nil {
		return fmt.Errorf("encode hub: %w", err)
	}
	if err := os.WriteFile(h.Path, raw, 0o600); err != nil {
		return fmt.Errorf("write hub: %w", err)
	}
	return nil
}

func sortEvents(events []state.Event) {
	sort.Slice(events, func(i, j int) bool {
		if events[i].HLC == events[j].HLC {
			if events[i].DeviceID == events[j].DeviceID {
				return events[i].ID < events[j].ID
			}
			return events[i].DeviceID < events[j].DeviceID
		}
		return events[i].HLC < events[j].HLC
	})
}
