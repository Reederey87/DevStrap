-- +goose Up
-- GIT-05: per-project forge override so self-hosted GitLab/Gitea/Forgejo
-- instances (git.acme.com, scm.internal) route to glab/tea instead of
-- degrading to a compare URL. Resolution order: --forge flag > this column >
-- [forge] host map > DetectForge heuristic. Empty string means "not set".
ALTER TABLE git_repos ADD COLUMN forge_kind TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE git_repos DROP COLUMN forge_kind;
