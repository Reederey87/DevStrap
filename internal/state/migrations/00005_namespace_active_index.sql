-- +goose Up
CREATE INDEX idx_namespace_active
  ON namespace_entries(workspace_id, path_key)
  WHERE status = 'active';

-- +goose Down
DROP INDEX idx_namespace_active;
