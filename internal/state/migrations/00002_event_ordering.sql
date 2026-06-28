-- +goose Up
ALTER TABLE namespace_entries ADD COLUMN tombstone_hlc INTEGER;

ALTER TABLE events ADD COLUMN hlc INTEGER NOT NULL DEFAULT 0;
ALTER TABLE events ADD COLUMN content_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE events ADD COLUMN device_sig TEXT;
ALTER TABLE events ADD COLUMN prev_event_hash TEXT;

CREATE INDEX idx_events_order ON events(workspace_id, hlc, device_id, id);
CREATE UNIQUE INDEX idx_events_device_seq ON events(device_id, seq) WHERE seq IS NOT NULL;

CREATE TABLE device_sync_state (
  device_id TEXT PRIMARY KEY,
  last_hlc INTEGER NOT NULL DEFAULT 0,
  next_seq INTEGER NOT NULL DEFAULT 1,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(device_id) REFERENCES devices(id) ON DELETE CASCADE
);

CREATE TABLE sync_cursors (
  workspace_id TEXT NOT NULL,
  peer_id TEXT NOT NULL,
  last_hlc_applied INTEGER NOT NULL DEFAULT 0,
  last_seq_applied INTEGER,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(workspace_id, peer_id),
  FOREIGN KEY(workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE,
  FOREIGN KEY(peer_id) REFERENCES devices(id) ON DELETE CASCADE
);

CREATE TABLE event_delivery (
  event_id TEXT NOT NULL,
  device_id TEXT NOT NULL,
  applied_at TEXT,
  sync_state TEXT NOT NULL DEFAULT 'pending',
  updated_at TEXT NOT NULL,
  PRIMARY KEY(event_id, device_id),
  FOREIGN KEY(event_id) REFERENCES events(id) ON DELETE CASCADE,
  FOREIGN KEY(device_id) REFERENCES devices(id) ON DELETE CASCADE
);

CREATE INDEX idx_event_delivery_state ON event_delivery(device_id, sync_state);

-- +goose Down
DROP INDEX idx_event_delivery_state;
DROP TABLE event_delivery;
DROP TABLE sync_cursors;
DROP TABLE device_sync_state;
DROP INDEX idx_events_device_seq;
DROP INDEX idx_events_order;
ALTER TABLE events DROP COLUMN prev_event_hash;
ALTER TABLE events DROP COLUMN device_sig;
ALTER TABLE events DROP COLUMN content_hash;
ALTER TABLE events DROP COLUMN hlc;
ALTER TABLE namespace_entries DROP COLUMN tombstone_hlc;
