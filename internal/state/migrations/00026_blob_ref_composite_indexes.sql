-- +goose Up
-- P7-DATA-06 (follow-up): the 00025 NOCASE indexes only serve the `col LIKE
-- 'age_blob:%'` enumeration scans; SQLite will not use a NOCASE index for a
-- BINARY-collation `col = ?` comparison. The revoke/rewrap loop
-- (EnvProfilesForBlobRef / DraftSnapshotsForBlobRef / UpdateBlobRef, driven per
-- distinct ref by blob_gc.go) filters/updates these columns with exact BINARY
-- equality, so add BINARY composite indexes leading with the equality column.
-- Partial on secret_bindings because encrypted_value_ref is NULL for provider
-- (op://) bindings, which the rewrap loop never touches.
CREATE INDEX idx_secret_bindings_env_profile_ref
  ON secret_bindings(encrypted_value_ref, env_profile_id)
  WHERE encrypted_value_ref IS NOT NULL;
CREATE INDEX idx_draft_snapshots_namespace_ref
  ON draft_snapshots(blob_ref, namespace_id);

-- +goose Down
DROP INDEX idx_draft_snapshots_namespace_ref;
DROP INDEX idx_secret_bindings_env_profile_ref;
