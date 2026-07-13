-- +goose Up
-- P7-SYNC-02: record the HLC of the revocation that first flipped a device to a
-- terminal trust state, so the apply path can time-scope trust checks — a
-- pre-revocation event (HLC below this boundary) from a now-revoked device is
-- admitted regardless of delivery order, while any event at or after the
-- boundary stays rejected. NULL means the boundary is unknown (fail closed).
ALTER TABLE devices ADD COLUMN revoked_at_hlc INTEGER;

-- +goose Down
ALTER TABLE devices DROP COLUMN revoked_at_hlc;
