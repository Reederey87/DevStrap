-- +goose Up
-- P4-SYNC-05: folded running hash chain + signed per-device head.
--
-- device_heads records, per ORIGIN device, the highest SIGNED head this device
-- has verified from that origin's ack marker (its promised seq + folded hash)
-- so a hub that withholds an origin's newest events is caught: the promise
-- commits to a seq beyond what we received, and a persistent shortfall is an
-- omission alarm. The row is per-peer observation state (a running watermark),
-- not derivable from the event log, so it is persisted here.
CREATE TABLE device_heads (
  workspace_id TEXT NOT NULL,
  device_id TEXT NOT NULL,
  promised_seq INTEGER NOT NULL,
  promised_fold TEXT NOT NULL,
  promised_hlc INTEGER NOT NULL,
  omission_pending INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (workspace_id, device_id)
);

-- The fold seed a snapshot-bootstrapped device needs to re-fold an origin's
-- stream from the retention floor (it holds no events below the floor). Mirrors
-- anchor_content_hash: the folded hash of the LAST event the snapshot covers for
-- that device (at seq = anchor_seq). Empty for pre-P4-SYNC-05 anchors, which
-- degrades omission detection for that origin to fail-safe (skipped, never a
-- false alarm).
ALTER TABLE sync_chain_anchors ADD COLUMN folded_hash TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE sync_chain_anchors DROP COLUMN folded_hash;
DROP TABLE device_heads;
