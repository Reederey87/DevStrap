-- +goose Up
-- P6-XP-04: local, never-synced key/value metadata for machine-local decisions
-- that must survive across runs but never ride the event log. The first
-- consumer is 'key_custody' — whether this machine keeps its device/workspace
-- secret material in the OS keychain ('keychain') or the 0600 file store
-- ('file'). The decision is recorded once at init from a one-time keychain
-- reachability probe and honored on every later run, so a store never silently
-- migrates custody backends (the split-custody wedge P6-XP-04 fixes). This
-- table holds no secret material and is intentionally not workspace-scoped: it
-- describes the host, not the synced namespace.
CREATE TABLE local_meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

-- +goose Down
DROP TABLE local_meta;
