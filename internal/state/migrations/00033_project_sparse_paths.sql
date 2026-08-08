-- +goose Up
-- W12-02: cone-mode git sparse-checkout profiles. A project can opt a subset
-- of its working tree into materialization (git's own working-tree/index-cost
-- reduction) on top of the existing blobless partial clone (transfer-cost
-- reduction) — the two are complementary, not alternatives (see
-- spec/08_GIT_MATERIALIZATION_AND_WORKTREES.md).
--
-- Deliberately LOCAL-ONLY, never synced through the signed event log: each
-- device chooses its own sparse profile, mirroring the pre-existing (and
-- still unused) git_repos.sparse_config TEXT column's documented intent —
-- "device re-derives sparse config on materialization" (internal/sync's
-- snapshot payload comment). No workspace_id (this table only needs to chain
-- through namespace_id -> namespace_entries -> workspaces) and no
-- source_event_* columns (nothing here is ever event-sourced or applied from
-- a peer). project_sparse_paths supersedes git_repos.sparse_config as the
-- actual storage for this feature; the column is left in place rather than
-- dropped since dropping a column already shipped in 00001_initial.sql is a
-- separate, higher-risk migration this change does not need to take on.
--
-- One row per configured cone directory, keyed by (namespace_id, path) so the
-- same directory cannot be recorded twice for one project; `path` is a plain
-- repo-relative directory (cone semantics only — see internal/git/sparse.go's
-- ValidSparsePath), never a non-cone glob pattern.
CREATE TABLE project_sparse_paths (
  namespace_id TEXT NOT NULL,
  path TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (namespace_id, path),
  FOREIGN KEY(namespace_id) REFERENCES namespace_entries(id) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE project_sparse_paths;
