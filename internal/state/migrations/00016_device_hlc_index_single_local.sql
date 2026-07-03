-- +goose Up
-- P6-DATA-05: serve the hot push/doctor query (device-scoped HLC scans).
CREATE INDEX idx_events_device_hlc ON events(device_id, hlc);
-- P6-DATA-06: enforce the single-local-device invariant at the schema layer.
-- Fails loudly if a store already violates it (two 'local' rows); remedy is
-- documented in spec/12 (delete the divergent row by hand -- device identity
-- is never auto-deleted).
CREATE UNIQUE INDEX idx_devices_single_local ON devices((1)) WHERE trust_state = 'local';

-- +goose Down
DROP INDEX idx_devices_single_local;
DROP INDEX idx_events_device_hlc;
