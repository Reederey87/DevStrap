-- +goose Up
CREATE UNIQUE INDEX idx_workspaces_singleton ON workspaces((1));

-- +goose Down
DROP INDEX idx_workspaces_singleton;
