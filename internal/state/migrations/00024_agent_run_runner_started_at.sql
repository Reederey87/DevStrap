-- +goose Up
-- P7-GIT-03: record the runner process's start-time identity alongside its PID
-- so a recycled PID cannot keep a crashed run 'running' forever.
ALTER TABLE agent_runs ADD COLUMN runner_started_at INTEGER;

-- +goose Down
ALTER TABLE agent_runs DROP COLUMN runner_started_at;
