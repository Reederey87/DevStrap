-- +goose Up
-- P6-SEC-02 / P6-SEC-01(b): key workspace_keys by (epoch, kid) instead of bare
-- epoch, so multiple WCKs can coexist at the same epoch (e.g. a founder key
-- and a still-unverified joiner key before reconciliation). kid is a
-- non-secret 64-lowercase-hex-char fingerprint (hex(sha256(wck))) computed
-- by the caller; the secret WCK itself still lives only in the OS
-- keychain / 0600 file fallback (devicekeys.HybridStore), never in SQLite.
-- origin records how this device came to hold the key: 'self' (founder
-- bootstrap or rotate), 'grant' (a verified device.key.granted event), or
-- 'legacy' (backfilled by this migration from a pre-kid row). The invariant
-- enforced at the call site (P6-SEC-01c) is that ONLY those three paths may
-- ever write a workspace_keys row.
--
-- Down is lossy: multiple kids recorded at the same epoch collapse back to a
-- single legacy row per (workspace_id, epoch), keeping the earliest
-- created_at. The grants table's kid column is dropped outright.
CREATE TABLE workspace_keys_new (
  workspace_id TEXT NOT NULL,
  epoch INTEGER NOT NULL,
  kid TEXT NOT NULL DEFAULT '',
  origin TEXT NOT NULL DEFAULT 'legacy' CHECK(origin IN ('self','grant','legacy')),
  created_at TEXT NOT NULL,
  PRIMARY KEY(workspace_id, epoch, kid),
  FOREIGN KEY(workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE
);

INSERT INTO workspace_keys_new (workspace_id, epoch, kid, origin, created_at)
SELECT workspace_id, epoch, '', 'legacy', created_at FROM workspace_keys;

DROP TABLE workspace_keys;
ALTER TABLE workspace_keys_new RENAME TO workspace_keys;

ALTER TABLE workspace_key_grants ADD COLUMN kid TEXT;

-- +goose Down
CREATE TABLE workspace_keys_old (
  workspace_id TEXT NOT NULL,
  epoch INTEGER NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY(workspace_id, epoch),
  FOREIGN KEY(workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE
);

INSERT INTO workspace_keys_old (workspace_id, epoch, created_at)
SELECT workspace_id, epoch, MIN(created_at)
FROM workspace_keys
GROUP BY workspace_id, epoch;

DROP TABLE workspace_keys;
ALTER TABLE workspace_keys_old RENAME TO workspace_keys;

ALTER TABLE workspace_key_grants DROP COLUMN kid;
