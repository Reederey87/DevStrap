-- +goose Up
-- P7-WIP-01: working-state validation plane Layer B — a signed, read-only
-- per-device record of a pushed WIP ref (repo.wip.pushed). Apply is
-- MIRROR-ONLY: each apply overwrites the row for (workspace_id, device_id,
-- path_key) with the latest push, never appending history.
--
-- No FK to devices: remote devices are not enrolled in the local device
-- registry until Phase 2 (an FK would break the first time this device
-- observes a WIP push from a peer it has not locally enrolled yet),
-- mirroring device_gitstate (00029) and device_heads (00028).
--
-- No FK to namespace_entries either: unlike env/draft pointer events, this
-- mirror does not need the pushed project to exist locally (there is no
-- pending-project quarantine class for this event type) — path/path_key are
-- stored verbatim as an opaque identifier, joined against namespace_entries
-- only by callers that need to (e.g. the future `wip status` CLI
-- surfacing), never required for the apply itself to succeed.
CREATE TABLE device_wip (
  workspace_id TEXT NOT NULL,
  device_id TEXT NOT NULL,
  path_key TEXT NOT NULL,
  path TEXT NOT NULL,
  ref TEXT NOT NULL DEFAULT '',
  sha TEXT NOT NULL DEFAULT '',
  base_sha TEXT NOT NULL DEFAULT '',
  captured_at TEXT NOT NULL DEFAULT '',
  observed_at_hlc INTEGER NOT NULL,
  source_event_id TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (workspace_id, device_id, path_key)
);

-- +goose Down
DROP TABLE device_wip;
