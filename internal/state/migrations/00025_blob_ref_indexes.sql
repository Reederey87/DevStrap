-- +goose Up
-- P7-DATA-06: index blob-reference columns so revoke/rewrap reference scans
-- (AllBlobRefs / EnvBlobRefs / DraftBlobRefs / RetainedBlobRefs) do not full-scan.
-- COLLATE NOCASE matches SQLite's default case-insensitive LIKE so the planner
-- can use these indexes for `col LIKE 'age_blob:%'`.
CREATE INDEX idx_secret_bindings_encrypted_value_ref ON secret_bindings(encrypted_value_ref COLLATE NOCASE);
CREATE INDEX idx_draft_snapshots_blob_ref ON draft_snapshots(blob_ref COLLATE NOCASE);

-- +goose Down
DROP INDEX idx_draft_snapshots_blob_ref;
DROP INDEX idx_secret_bindings_encrypted_value_ref;
