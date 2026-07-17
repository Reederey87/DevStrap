-- +goose Up
-- P7-GITSTATE-01: working-state validation plane Layer A — a signed,
-- read-only per-device git-state snapshot (repo.gitstate.observed). Apply is
-- MIRROR-ONLY: each apply overwrites the row for (workspace_id, device_id,
-- path_key) with the latest observation, never appending history.
--
-- No FK to devices: remote devices are not enrolled in the local device
-- registry until Phase 2 (an FK would break the first time this device
-- observes gitstate from a peer it has not locally enrolled yet), mirroring
-- device_heads (00028).
--
-- No FK to namespace_entries either: unlike env/draft pointer events, this
-- mirror does not need the observed project to exist locally (there is no
-- pending-project quarantine class for this event type) — path/path_key are
-- stored verbatim as an opaque identifier, joined against namespace_entries
-- only by callers that need to (e.g. the future `status --all-devices` CLI
-- surfacing), never required for the apply itself to succeed.
CREATE TABLE device_gitstate (
  workspace_id TEXT NOT NULL,
  device_id TEXT NOT NULL,
  path_key TEXT NOT NULL,
  path TEXT NOT NULL,
  branch TEXT NOT NULL DEFAULT '',
  head_sha TEXT NOT NULL DEFAULT '',
  upstream_branch TEXT NOT NULL DEFAULT '',
  upstream_sha TEXT NOT NULL DEFAULT '',
  dirty_count INTEGER NOT NULL DEFAULT 0,
  untracked_count INTEGER NOT NULL DEFAULT 0,
  unmerged_count INTEGER NOT NULL DEFAULT 0,
  ahead_count INTEGER NOT NULL DEFAULT 0,
  behind_count INTEGER NOT NULL DEFAULT 0,
  stash_count INTEGER NOT NULL DEFAULT 0,
  observed_at_hlc INTEGER NOT NULL,
  source_event_id TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (workspace_id, device_id, path_key)
);

-- +goose Down
DROP TABLE device_gitstate;
