-- +goose Up
-- DRAFT-02: encrypted draft bundle snapshots. Each draft.snapshot.created event
-- records a content-addressed age_blob:<sha256> bundle for a non-git project.
-- draft_projects.current_snapshot_id points at the latest applied snapshot.
CREATE TABLE draft_snapshots (
  id TEXT PRIMARY KEY,
  namespace_id TEXT NOT NULL,
  blob_ref TEXT NOT NULL,
  byte_size INTEGER NOT NULL DEFAULT 0,
  file_count INTEGER NOT NULL DEFAULT 0,
  source_event_hlc INTEGER,
  source_event_device_id TEXT,
  source_event_id TEXT,
  created_at TEXT NOT NULL,
  FOREIGN KEY(namespace_id) REFERENCES namespace_entries(id) ON DELETE CASCADE
);

CREATE INDEX idx_draft_snapshots_namespace ON draft_snapshots(namespace_id);

-- +goose Down
DROP INDEX idx_draft_snapshots_namespace;
DROP TABLE draft_snapshots;
