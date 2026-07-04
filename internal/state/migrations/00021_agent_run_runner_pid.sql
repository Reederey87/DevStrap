-- +goose Up
-- P6-GIT-06: record the CLI process that owns an in-flight agent run so
-- crash-stuck running rows can be reconciled after the process is gone.
ALTER TABLE agent_runs ADD COLUMN runner_pid INTEGER;

-- +goose Down
ALTER TABLE agent_runs DROP COLUMN runner_pid;
