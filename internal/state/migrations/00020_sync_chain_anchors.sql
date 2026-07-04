-- +goose Up
-- P4-SYNC-02: per-device hash-chain anchors imported from a snapshot. A
-- snapshot-bootstrapped device has no event rows below the retention floor,
-- so the prev-hash verification of the first post-floor event per device
-- falls back to its anchor (the content hash of the last covered event).
CREATE TABLE sync_chain_anchors (
  workspace_id TEXT NOT NULL,
  device_id TEXT NOT NULL,
  anchor_seq INTEGER NOT NULL,
  anchor_content_hash TEXT NOT NULL,
  anchor_hlc INTEGER NOT NULL,
  snapshot_sha256 TEXT NOT NULL,
  imported_at TEXT NOT NULL,
  PRIMARY KEY (workspace_id, device_id)
);
-- +goose Down
DROP TABLE sync_chain_anchors;
