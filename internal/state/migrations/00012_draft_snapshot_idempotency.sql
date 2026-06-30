-- +goose Up
-- P5-DATA-02: enforce draft-snapshot idempotency at the DB layer, not only via
-- the SELECT-then-INSERT guard in Go, mirroring how the events table protects
-- idempotency (PK + INSERT OR IGNORE). The partial index covers only real
-- (non-empty) source_event_ids, so multiple unsourced rows remain allowed.
CREATE UNIQUE INDEX idx_draft_snapshots_source_event
  ON draft_snapshots(namespace_id, source_event_id)
  WHERE source_event_id IS NOT NULL AND source_event_id != '';

-- +goose Down
DROP INDEX idx_draft_snapshots_source_event;
