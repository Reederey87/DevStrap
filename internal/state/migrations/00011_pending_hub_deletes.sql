-- +goose Up
-- P5-PROD-02 / P5-SEC-01: blobs orphaned by a local-only revoke rewrap (no hub
-- configured at revoke time) are queued here and deleted from the hub on the
-- next sync/hub-gc that has a hub. This makes the cleanup promise real instead
-- of a note that nothing ever fulfills.
CREATE TABLE pending_hub_deletes (
  workspace_id TEXT NOT NULL,
  blob_ref TEXT NOT NULL,
  queued_at TEXT NOT NULL,
  PRIMARY KEY (workspace_id, blob_ref),
  FOREIGN KEY(workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE pending_hub_deletes;
