-- +goose Up
-- P6-SEC-03: grace-window bookkeeping for not-yet-granted workspace keys.
-- When EncryptedHub.Pull first defers (truncates) on an event sealed under an
-- (epoch, kid) this device does not hold, a row here records WHEN that key was
-- first found missing. The first_seen_at timestamp is the stable start of the
-- grace window (sync.key_grant_grace): within the window the pull keeps
-- truncating (the grant is probably in flight); past it the still-encrypted
-- carrier is forwarded for a permanent undecryptable quarantine so the cursor
-- can advance and sync is no longer wedged forever. Rows are cleared by
-- RecordKeyEpoch the moment a matching key is finally held (the wait is over);
-- rows that never clear are surfaced by `doctor` as "awaiting key grants".
-- kid may be '' (a whole epoch is missing) or a specific unheld kid at a held
-- epoch (the P6-SEC-02 collision case). No secret material is stored here.
CREATE TABLE key_grant_waits (
  workspace_id TEXT NOT NULL,
  epoch INTEGER NOT NULL,
  kid TEXT NOT NULL DEFAULT '',
  first_seen_at TEXT NOT NULL,
  PRIMARY KEY(workspace_id, epoch, kid),
  FOREIGN KEY(workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE key_grant_waits;
