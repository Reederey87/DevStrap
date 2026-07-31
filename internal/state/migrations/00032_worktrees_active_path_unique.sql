-- +goose Up
-- AD5-02: `devstrap worktree adopt` registers an externally-created linked
-- worktree (Codex/Cursor/Devin create their own worktrees behind DevStrap's
-- back). Nothing before this migration stops the same physical worktree path
-- from being registered twice — `worktrees` has never had a uniqueness
-- constraint on path (00001_initial.sql, never ALTERed) — so a re-run of
-- `adopt` (or `worktree new` racing an adopt of the same checkout) would
-- silently create a second row pointing at the identical directory, and the
-- registry's "one row per worktree" invariant that the stale-base gate and
-- `agent pr` provenance depend on would quietly stop holding.
--
-- The predicate is scoped to `status = 'active'` rather than the bare
-- (namespace_id, path) pair: a removed worktree's path is legitimately
-- reusable (the checkout is gone; a later `adopt` or `worktree new` at the
-- same path is a fresh registration, not a duplicate of dead history).
CREATE UNIQUE INDEX idx_worktrees_active_path ON worktrees(namespace_id, path) WHERE status = 'active';

-- +goose Down
DROP INDEX idx_worktrees_active_path;
