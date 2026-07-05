-- +goose Up
-- P4-GIT-03 slice 5: unsigned local telemetry for the OS agent sandbox. The
-- three agent_runs columns record which backend/mode/limitations a run used
-- (empty for unsandboxed runs). sandbox_violations is the visibility layer for
-- kernel denials the macOS Seatbelt backend reports post-run (coordinates +
-- reason only, no secret material — matching sync_skipped_events). This is
-- NOT the signed audit_log (still unbuilt, spec/15); it is best-effort local
-- visibility surfaced by `agent show` and `doctor`.
ALTER TABLE agent_runs ADD COLUMN sandbox_backend TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_runs ADD COLUMN sandbox_mode TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_runs ADD COLUMN sandbox_limitations TEXT NOT NULL DEFAULT '';

CREATE TABLE sandbox_violations (
  run_id TEXT NOT NULL,
  observed_at TEXT NOT NULL,
  backend TEXT NOT NULL,
  operation TEXT NOT NULL,
  path TEXT NOT NULL DEFAULT '',
  detail TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL,
  FOREIGN KEY (run_id) REFERENCES agent_runs(id) ON DELETE CASCADE
);
CREATE INDEX idx_sandbox_violations_run ON sandbox_violations(run_id);

-- +goose Down
DROP TABLE sandbox_violations;
ALTER TABLE agent_runs DROP COLUMN sandbox_limitations;
ALTER TABLE agent_runs DROP COLUMN sandbox_mode;
ALTER TABLE agent_runs DROP COLUMN sandbox_backend;
