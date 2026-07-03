-- +goose Up
-- P6-SYNC-02: durable record for events EncryptedHub.Pull cannot classify
-- into apply/quarantine and must drop from the batch (unknown envelope
-- version, retired enc.v1, anti-downgrade plaintext). Under the per-device
-- Seq cursor (00017) a dropped event leaves a seq gap that HOLDS its origin
-- device's cursor, so these rows are the visibility layer for that retry
-- wedge: surfaced by status/doctor, consulted by hub gc's refuse-to-sweep,
-- and first_seen_at is the grace clock for the recoverable class (unknown
-- version defers per-device until the operator upgrades or the grace lapses
-- into the undecryptable quarantine). Rows clear when their event finally
-- applies. No secret material is stored (id/coordinates/reason only).
CREATE TABLE sync_skipped_events (
  workspace_id TEXT NOT NULL,
  event_id TEXT NOT NULL,
  device_id TEXT NOT NULL DEFAULT '',
  seq INTEGER NOT NULL DEFAULT 0,
  hlc INTEGER NOT NULL DEFAULT 0,
  reason TEXT NOT NULL,
  first_seen_at TEXT NOT NULL,
  PRIMARY KEY (workspace_id, event_id, reason),
  FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE sync_skipped_events;
