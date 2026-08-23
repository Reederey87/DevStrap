-- +goose Up
-- W15-01: cache the local sparse-profile directory-existence probe. The
-- answer is a pure function of one project's desired normalized cone paths
-- and one resolved Git commit tree, so one replaceable row per project is
-- enough. This is local performance/warning-dedup state, never event-sourced
-- or synced; it contains no repository content or secret material.
CREATE TABLE project_sparse_probe_cache (
  namespace_id TEXT PRIMARY KEY,
  head_sha TEXT NOT NULL,
  profile_hash TEXT NOT NULL,
  missing_paths_json TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(namespace_id) REFERENCES namespace_entries(id) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE project_sparse_probe_cache;
