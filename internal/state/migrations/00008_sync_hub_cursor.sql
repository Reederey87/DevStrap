-- +goose Up
-- EAGER-02: cursor-based incremental hub pull. Tracks the max HLC applied from
-- a given hub source so the next Pull requests only events newer than the last
-- applied, instead of full-history replay every sync. hub_id is a free-form hub
-- identifier (e.g. "file:<path>" or "r2:<workspace_id>") with no FK to devices,
-- because a hub aggregates events from many devices.
CREATE TABLE hub_cursors (
  workspace_id TEXT NOT NULL,
  hub_id TEXT NOT NULL,
  last_hlc_applied INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(workspace_id, hub_id),
  FOREIGN KEY(workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE
);

CREATE INDEX idx_hub_cursors_workspace ON hub_cursors(workspace_id);

-- +goose Down
DROP INDEX idx_hub_cursors_workspace;
DROP TABLE hub_cursors;
