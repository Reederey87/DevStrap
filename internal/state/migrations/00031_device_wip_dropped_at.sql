-- +goose Up
-- P7-WIP-06: repo.wip.dropped propagates successful leased ref deletions
-- fleet-wide. A drop is retained as a tombstone rather than deleting the
-- device_wip row so an older repo.wip.pushed event delivered later cannot
-- resurrect a ref that is already gone.
--
-- dropped_at_hlc is the drop event's ordering coordinate; dropped_sha is the
-- exact old ref value against which git's --force-with-lease delete
-- succeeded. Reads use both values: an observation newer than the tombstone
-- is live, and so is a different-SHA observation at any HLC because the lease
-- proves the drop deleted only dropped_sha and a racing push may have won
-- afterward in a different device stream.
--
-- Tombstones are permanent. Cardinality is bounded by devices × projects,
-- while purging would reopen the resurrection window for pushed events still
-- below the hub retention floor. device_wip participates in neither snapshot
-- exchange nor hub-compaction serialization, so those channels cannot
-- resurrect a row either.
ALTER TABLE device_wip ADD COLUMN dropped_at_hlc INTEGER;
ALTER TABLE device_wip ADD COLUMN dropped_sha TEXT;

-- +goose Down
-- Drop the DEAD rows before rebuilding. Schema 30 encodes "this WIP ref was
-- dropped" as row ABSENCE, so carrying tombstoned rows across the downgrade
-- resurrects them as fully live-looking rows — and not only the harmless
-- tombstone-only ones: a normally-dropped row downgrades carrying a REAL sha
-- for a ref that is gone from the remote, so the fleet's whole drop history
-- re-surfaces in `wip status`/`doctor` and `wip apply` then fails on fetch.
-- Losing the tombstone itself is not a regression: these columns are lost
-- either way, and absence is schema 30's correct encoding of the same fact.
-- This predicate must stay in sync with state.deviceWipLivePredicate.
DELETE FROM device_wip
WHERE NOT (
  dropped_at_hlc IS NULL
  OR observed_at_hlc > dropped_at_hlc
  OR (observed_at_hlc > 0 AND COALESCE(dropped_sha, '') <> '' AND sha <> dropped_sha)
);

-- SQLite at DevStrap's supported version floor cannot portably drop columns,
-- so rebuild the schema-30 table while preserving every pre-00031 field.
CREATE TABLE device_wip_00030 (
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

INSERT INTO device_wip_00030 (
  workspace_id, device_id, path_key, path, ref, sha, base_sha, captured_at,
  observed_at_hlc, source_event_id, updated_at
)
SELECT
  workspace_id, device_id, path_key, path, ref, sha, base_sha, captured_at,
  observed_at_hlc, source_event_id, updated_at
FROM device_wip;

DROP TABLE device_wip;
ALTER TABLE device_wip_00030 RENAME TO device_wip;
