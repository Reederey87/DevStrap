-- +goose Up
-- P5-SYNC-01: decouple the sync transport cursor from the HLC clock. The pull
-- resumes per origin device at its contiguous per-device seq, so an event
-- pushed late (with an old HLC) can never be skipped by an HLC watermark.
-- device_id is the hub-observed origin device; deliberately NO FK to devices —
-- a cursor may advance for a device whose events all quarantined and that was
-- never enrolled locally (same rationale as hub_cursors' free-form hub_id,
-- migration 00008). The push watermark reuses this table as a
-- (hub_id = 'push:<hubID>', device_id = <local device>) row. hub_cursors
-- (00008) is retained frozen for the founder-gate legacy check and the
-- one-time push-watermark backfill.
CREATE TABLE hub_device_cursors (
  workspace_id TEXT NOT NULL,
  hub_id TEXT NOT NULL,
  device_id TEXT NOT NULL,
  last_seq_pulled INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (workspace_id, hub_id, device_id),
  FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE
);
CREATE INDEX idx_hub_device_cursors_hub ON hub_device_cursors(workspace_id, hub_id);

-- +goose Down
DROP INDEX idx_hub_device_cursors_hub;
DROP TABLE hub_device_cursors;
