-- +goose Up
CREATE TABLE workspaces (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  root_path TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE devices (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  os TEXT NOT NULL,
  arch TEXT NOT NULL,
  hostname TEXT,
  public_key TEXT,
  trust_state TEXT NOT NULL DEFAULT 'pending',
  last_seen_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE namespace_entries (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL,
  path TEXT NOT NULL,
  path_key TEXT NOT NULL,
  type TEXT NOT NULL,
  display_name TEXT,
  materialization_policy TEXT NOT NULL DEFAULT 'lazy',
  env_profile_id TEXT,
  tooling_profile_id TEXT,
  agent_policy_id TEXT,
  ignore_profile_id TEXT,
  status TEXT NOT NULL DEFAULT 'active',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(workspace_id, path_key),
  FOREIGN KEY(workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE
);

CREATE TABLE git_repos (
  namespace_id TEXT PRIMARY KEY,
  remote_url TEXT NOT NULL,
  remote_key TEXT NOT NULL,
  default_branch TEXT NOT NULL DEFAULT 'main',
  clone_filter TEXT,
  sparse_config TEXT,
  lfs_policy TEXT NOT NULL DEFAULT 'auto',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(namespace_id) REFERENCES namespace_entries(id) ON DELETE CASCADE
);

CREATE TABLE draft_projects (
  namespace_id TEXT PRIMARY KEY,
  current_snapshot_id TEXT,
  max_bytes INTEGER NOT NULL DEFAULT 104857600,
  max_files INTEGER NOT NULL DEFAULT 5000,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(namespace_id) REFERENCES namespace_entries(id) ON DELETE CASCADE
);

CREATE TABLE device_project_state (
  device_id TEXT NOT NULL,
  namespace_id TEXT NOT NULL,
  local_path TEXT NOT NULL,
  materialization_state TEXT NOT NULL DEFAULT 'skeleton',
  git_branch TEXT,
  git_head_sha TEXT,
  git_upstream_sha TEXT,
  dirty_state TEXT NOT NULL DEFAULT 'unknown',
  env_ready INTEGER NOT NULL DEFAULT 0,
  tooling_ready INTEGER NOT NULL DEFAULT 0,
  last_scan_at TEXT,
  last_fetch_at TEXT,
  last_error TEXT,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(device_id, namespace_id),
  FOREIGN KEY(device_id) REFERENCES devices(id) ON DELETE CASCADE,
  FOREIGN KEY(namespace_id) REFERENCES namespace_entries(id) ON DELETE CASCADE
);

CREATE TABLE env_profiles (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL,
  name TEXT NOT NULL,
  provider TEXT NOT NULL,
  mode TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE
);

CREATE TABLE secret_bindings (
  id TEXT PRIMARY KEY,
  env_profile_id TEXT NOT NULL,
  var_name TEXT NOT NULL,
  provider_ref TEXT,
  encrypted_value_ref TEXT,
  required INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(env_profile_id, var_name),
  CHECK ((provider_ref IS NOT NULL) <> (encrypted_value_ref IS NOT NULL)),
  CHECK (encrypted_value_ref IS NULL OR encrypted_value_ref LIKE 'age_blob:%'),
  FOREIGN KEY(env_profile_id) REFERENCES env_profiles(id) ON DELETE CASCADE
);

CREATE TABLE worktrees (
  id TEXT PRIMARY KEY,
  namespace_id TEXT NOT NULL,
  device_id TEXT NOT NULL,
  path TEXT NOT NULL,
  branch TEXT NOT NULL,
  base_ref TEXT NOT NULL,
  base_sha TEXT NOT NULL,
  created_by TEXT NOT NULL,
  agent_run_id TEXT,
  status TEXT NOT NULL DEFAULT 'active',
  dirty_state TEXT NOT NULL DEFAULT 'unknown',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(namespace_id) REFERENCES namespace_entries(id) ON DELETE CASCADE,
  FOREIGN KEY(device_id) REFERENCES devices(id) ON DELETE CASCADE
);

CREATE TABLE agent_runs (
  id TEXT PRIMARY KEY,
  namespace_id TEXT NOT NULL,
  worktree_id TEXT,
  engine TEXT NOT NULL,
  task TEXT NOT NULL,
  policy_id TEXT,
  status TEXT NOT NULL DEFAULT 'pending',
  base_ref TEXT,
  base_sha TEXT,
  branch TEXT,
  log_path TEXT,
  diff_summary TEXT,
  test_summary TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(namespace_id) REFERENCES namespace_entries(id) ON DELETE CASCADE,
  FOREIGN KEY(worktree_id) REFERENCES worktrees(id) ON DELETE SET NULL
);

CREATE TABLE events (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL,
  device_id TEXT NOT NULL,
  seq INTEGER,
  type TEXT NOT NULL,
  payload_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  FOREIGN KEY(workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE,
  FOREIGN KEY(device_id) REFERENCES devices(id) ON DELETE CASCADE
);

CREATE TABLE jobs (
  id TEXT PRIMARY KEY,
  type TEXT NOT NULL,
  namespace_id TEXT,
  status TEXT NOT NULL DEFAULT 'queued',
  priority INTEGER NOT NULL DEFAULT 100,
  payload_json TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT,
  run_after TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(namespace_id) REFERENCES namespace_entries(id) ON DELETE SET NULL
);

CREATE TABLE conflicts (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL,
  namespace_id TEXT,
  type TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'open',
  details_json TEXT NOT NULL,
  resolution_json TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE,
  FOREIGN KEY(namespace_id) REFERENCES namespace_entries(id) ON DELETE SET NULL
);

CREATE INDEX idx_git_remote_key ON git_repos(remote_key);
CREATE INDEX idx_device_state_namespace ON device_project_state(namespace_id);
CREATE INDEX idx_jobs_status_priority ON jobs(status, priority, run_after);
CREATE INDEX idx_worktrees_namespace ON worktrees(namespace_id);
CREATE INDEX idx_agent_runs_status ON agent_runs(status);
CREATE INDEX idx_secret_bindings_profile ON secret_bindings(env_profile_id);
CREATE INDEX idx_env_profiles_workspace ON env_profiles(workspace_id);
CREATE INDEX idx_worktrees_device ON worktrees(device_id);
CREATE INDEX idx_agent_runs_namespace ON agent_runs(namespace_id);
CREATE INDEX idx_jobs_namespace ON jobs(namespace_id);
CREATE INDEX idx_conflicts_namespace ON conflicts(namespace_id);

-- +goose Down
DROP TABLE conflicts;
DROP TABLE jobs;
DROP TABLE events;
DROP TABLE agent_runs;
DROP TABLE worktrees;
DROP TABLE secret_bindings;
DROP TABLE env_profiles;
DROP TABLE device_project_state;
DROP TABLE draft_projects;
DROP TABLE git_repos;
DROP TABLE namespace_entries;
DROP TABLE devices;
DROP TABLE workspaces;
