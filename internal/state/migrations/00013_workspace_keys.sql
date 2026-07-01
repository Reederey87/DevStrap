-- +goose Up
-- P4-SEC-07 / P4-SEC-02: workspace content key (WCK) epoch metadata for
-- envelope encryption of the namespace-map event log. The secret 32-byte WCK
-- per epoch lives ONLY in the OS keychain / 0600 file fallback
-- (devicekeys.HybridStore StoreWCK/LoadWCK), never in SQLite. These tables hold
-- non-secret metadata only: which epochs this device holds a WCK for, and an
-- audit trail of which age recipients were granted which epoch. Grant payloads
-- (the age-wrapped WCK) ride the event log as device.key.granted events, not
-- here.
CREATE TABLE workspace_keys (
  workspace_id TEXT NOT NULL,
  epoch INTEGER NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY(workspace_id, epoch),
  FOREIGN KEY(workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE
);

CREATE TABLE workspace_key_grants (
  workspace_id TEXT NOT NULL,
  epoch INTEGER NOT NULL,
  recipient TEXT NOT NULL,
  source_event_id TEXT NOT NULL,
  source_event_hlc INTEGER,
  source_event_device_id TEXT,
  created_at TEXT NOT NULL,
  PRIMARY KEY(workspace_id, epoch, recipient),
  FOREIGN KEY(workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE
);

CREATE INDEX idx_workspace_key_grants_epoch ON workspace_key_grants(workspace_id, epoch);

-- +goose Down
DROP INDEX idx_workspace_key_grants_epoch;
DROP TABLE workspace_key_grants;
DROP TABLE workspace_keys;
