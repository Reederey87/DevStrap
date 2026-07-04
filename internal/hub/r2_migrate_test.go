package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
)

// seedLegacy writes an event under the retired HLC-keyed layout and returns its
// key. Mirrors the layout the dual-read parses.
func seedLegacy(t *testing.T, ctx context.Context, mem *memS3, wsID string, e state.Event) string {
	t.Helper()
	raw, _ := json.Marshal(e)
	key := fmt.Sprintf("workspaces/%s/events/%020d/%s/%d/%s.json", wsID, e.HLC, e.DeviceID, e.Seq, e.ID)
	if err := mem.PutObject(ctx, key, raw, true); err != nil {
		t.Fatalf("seed legacy object: %v", err)
	}
	return key
}

func legacyKeysPresent(t *testing.T, ctx context.Context, mem *memS3, wsID string) []string {
	t.Helper()
	objs, _, err := mem.ListObjectsV2(ctx, fmt.Sprintf("workspaces/%s/events/", wsID), "", 1000)
	if err != nil {
		t.Fatalf("list legacy: %v", err)
	}
	var keys []string
	for _, o := range objs {
		keys = append(keys, o.Key)
	}
	return keys
}

// TestMigrateLegacyEventsFullyMigratesAndEmptiesPrefix: a hub with only
// parseable legacy objects migrates them all, the legacy prefix is empty
// afterward, and every object is now readable at the new seq layout.
func TestMigrateLegacyEventsFullyMigratesAndEmptiesPrefix(t *testing.T) {
	ctx := context.Background()
	h := newTestR2Hub(t)
	mem := h.S3.(*memS3)
	eA1 := makeEvent("evt_a1", "dev_a", 100, 1, "project.added", `{"path":"a1"}`)
	eA2 := makeEvent("evt_a2", "dev_a", 200, 2, "project.updated", `{"path":"a2"}`)
	eB1 := makeEvent("evt_b1", "dev_b", 150, 1, "project.added", `{"path":"b1"}`)
	seedLegacy(t, ctx, mem, h.WorkspaceID, eA1)
	seedLegacy(t, ctx, mem, h.WorkspaceID, eA2)
	seedLegacy(t, ctx, mem, h.WorkspaceID, eB1)

	migrated, kept, err := h.MigrateLegacyEvents(ctx, false)
	if err != nil {
		t.Fatalf("MigrateLegacyEvents: %v", err)
	}
	if migrated != 3 || kept != 0 {
		t.Fatalf("MigrateLegacyEvents = (%d, %d), want (3, 0)", migrated, kept)
	}
	if keys := legacyKeysPresent(t, ctx, mem, h.WorkspaceID); len(keys) != 0 {
		t.Fatalf("legacy prefix not empty after migration: %v", keys)
	}
	// Every migrated event is now readable through the seq layout.
	pulled, err := h.Pull(ctx, nil)
	if err != nil {
		t.Fatalf("Pull after migration: %v", err)
	}
	if len(pulled) != 3 {
		t.Fatalf("Pull after migration = %d events, want 3", len(pulled))
	}
}

// TestMigrateLegacyEventsKeepsUnparseableAndMismatched: an unparseable key and a
// body whose (device, seq) disagree with the key are reported and KEPT.
func TestMigrateLegacyEventsKeepsUnparseableAndMismatched(t *testing.T) {
	ctx := context.Background()
	h := newTestR2Hub(t)
	mem := h.S3.(*memS3)

	good := makeEvent("evt_good", "dev_a", 100, 1, "project.added", `{"path":"g"}`)
	seedLegacy(t, ctx, mem, h.WorkspaceID, good)

	// Unparseable key: missing segments.
	oddRaw, _ := json.Marshal(makeEvent("evt_odd", "dev_b", 150, 1, "project.added", `{"path":"o"}`))
	oddKey := fmt.Sprintf("workspaces/%s/events/strange-key.json", h.WorkspaceID)
	if err := mem.PutObject(ctx, oddKey, oddRaw, true); err != nil {
		t.Fatalf("seed oddball: %v", err)
	}

	// Coordinate mismatch: key says dev_c/seq 5, body says dev_c/seq 9.
	mismatch := makeEvent("evt_mm", "dev_c", 300, 9, "project.added", `{"path":"m"}`)
	mmRaw, _ := json.Marshal(mismatch)
	mmKey := fmt.Sprintf("workspaces/%s/events/%020d/%s/%d/%s.json", h.WorkspaceID, mismatch.HLC, "dev_c", 5, mismatch.ID)
	if err := mem.PutObject(ctx, mmKey, mmRaw, true); err != nil {
		t.Fatalf("seed mismatch: %v", err)
	}

	migrated, kept, err := h.MigrateLegacyEvents(ctx, false)
	if err != nil {
		t.Fatalf("MigrateLegacyEvents: %v", err)
	}
	if migrated != 1 || kept != 2 {
		t.Fatalf("MigrateLegacyEvents = (%d, %d), want (1, 2)", migrated, kept)
	}
	present := legacyKeysPresent(t, ctx, mem, h.WorkspaceID)
	if len(present) != 2 {
		t.Fatalf("kept legacy objects = %v, want 2 (oddball + mismatch)", present)
	}
	for _, k := range present {
		if k != oddKey && k != mmKey {
			t.Fatalf("unexpected kept key %q", k)
		}
	}
}

// TestMigrateLegacyEventsIdempotentAndResumable: a second run reports nothing to
// migrate, and re-adding one legacy object (simulating an interrupted migration
// or a straggling old writer) converges on the next run.
func TestMigrateLegacyEventsIdempotentAndResumable(t *testing.T) {
	ctx := context.Background()
	h := newTestR2Hub(t)
	mem := h.S3.(*memS3)
	e := makeEvent("evt_a1", "dev_a", 100, 1, "project.added", `{"path":"a1"}`)
	seedLegacy(t, ctx, mem, h.WorkspaceID, e)

	if m, k, err := h.MigrateLegacyEvents(ctx, false); err != nil || m != 1 || k != 0 {
		t.Fatalf("first run = (%d, %d, %v), want (1, 0, nil)", m, k, err)
	}
	// Re-run: nothing left to migrate.
	if m, k, err := h.MigrateLegacyEvents(ctx, false); err != nil || m != 0 || k != 0 {
		t.Fatalf("re-run = (%d, %d, %v), want (0, 0, nil)", m, k, err)
	}
	// Interrupted-migration simulation: the new-key object already exists (from
	// the first run) and a legacy twin reappears. The re-run 412s on the PUT,
	// verifies the read-back, and deletes the straggler.
	seedLegacy(t, ctx, mem, h.WorkspaceID, e)
	if m, k, err := h.MigrateLegacyEvents(ctx, false); err != nil || m != 1 || k != 0 {
		t.Fatalf("resume run = (%d, %d, %v), want (1, 0, nil)", m, k, err)
	}
	if keys := legacyKeysPresent(t, ctx, mem, h.WorkspaceID); len(keys) != 0 {
		t.Fatalf("legacy prefix not empty after resume: %v", keys)
	}
}

// TestMigrateLegacyEventsDryRunWritesNothing: dry-run classifies but neither
// writes the new key nor deletes the legacy object.
func TestMigrateLegacyEventsDryRunWritesNothing(t *testing.T) {
	ctx := context.Background()
	h := newTestR2Hub(t)
	mem := h.S3.(*memS3)
	e := makeEvent("evt_a1", "dev_a", 100, 1, "project.added", `{"path":"a1"}`)
	legacyKey := seedLegacy(t, ctx, mem, h.WorkspaceID, e)

	m, k, err := h.MigrateLegacyEvents(ctx, true)
	if err != nil || m != 1 || k != 0 {
		t.Fatalf("dry-run = (%d, %d, %v), want (1, 0, nil)", m, k, err)
	}
	// Legacy object still present, new key absent.
	if _, err := mem.GetObject(ctx, legacyKey); err != nil {
		t.Fatalf("dry-run deleted the legacy object: %v", err)
	}
	if _, err := mem.GetObject(ctx, h.eventKey(e)); err == nil {
		t.Fatalf("dry-run wrote the new-key object")
	}
}

// TestMigrateLegacyEventsReadbackFailureKeepsLegacy: a backend that returns
// wrong bytes on the new key must NOT delete the legacy object (fail safe).
func TestMigrateLegacyEventsReadbackFailureKeepsLegacy(t *testing.T) {
	ctx := context.Background()
	mem := newMemS3()
	h := R2Hub{S3: &wrongReadbackS3{memS3: mem}, WorkspaceID: "ws_test"}
	e := makeEvent("evt_a1", "dev_a", 100, 1, "project.added", `{"path":"a1"}`)
	legacyKey := seedLegacy(t, ctx, mem, h.WorkspaceID, e)

	m, k, err := h.MigrateLegacyEvents(ctx, false)
	if err != nil {
		t.Fatalf("MigrateLegacyEvents: %v", err)
	}
	if m != 0 || k != 1 {
		t.Fatalf("MigrateLegacyEvents = (%d, %d), want (0, 1) — readback mismatch keeps", m, k)
	}
	if _, gerr := mem.GetObject(ctx, legacyKey); gerr != nil {
		t.Fatalf("legacy object deleted despite readback mismatch: %v", gerr)
	}
}

// TestMigrateThenPullEquivalence: a Pull returns the same events before and
// after migration (dual-read parity), so migration is transparent to consumers.
func TestMigrateThenPullEquivalence(t *testing.T) {
	ctx := context.Background()
	h := newTestR2Hub(t)
	mem := h.S3.(*memS3)
	events := []state.Event{
		makeEvent("evt_a1", "dev_a", 100, 1, "project.added", `{"path":"a1"}`),
		makeEvent("evt_a2", "dev_a", 200, 2, "project.updated", `{"path":"a2"}`),
		makeEvent("evt_b1", "dev_b", 150, 1, "project.added", `{"path":"b1"}`),
	}
	for _, e := range events {
		seedLegacy(t, ctx, mem, h.WorkspaceID, e)
	}
	before, err := h.Pull(ctx, nil)
	if err != nil {
		t.Fatalf("Pull before: %v", err)
	}
	if _, _, err := h.MigrateLegacyEvents(ctx, false); err != nil {
		t.Fatalf("MigrateLegacyEvents: %v", err)
	}
	after, err := h.Pull(ctx, nil)
	if err != nil {
		t.Fatalf("Pull after: %v", err)
	}
	if !eventIDsEqual(before, after) {
		t.Fatalf("Pull result changed across migration:\n before=%v\n after=%v", ids(before), ids(after))
	}
}

func ids(events []state.Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.ID)
	}
	return out
}

func eventIDsEqual(a, b []state.Event) bool {
	return strings.Join(ids(a), ",") == strings.Join(ids(b), ",")
}

// wrongReadbackS3 wraps memS3 and returns corrupted bytes for GetObject on any
// seq-layout (eventlog/) key, so the migration's read-back verification fails.
type wrongReadbackS3 struct {
	*memS3
}

func (w *wrongReadbackS3) GetObject(ctx context.Context, key string) ([]byte, error) {
	data, err := w.memS3.GetObject(ctx, key)
	if err != nil {
		return nil, err
	}
	if strings.Contains(key, "/eventlog/") {
		return append([]byte("corrupt"), data...), nil
	}
	return data, nil
}

var _ S3Client = (*wrongReadbackS3)(nil)
