-- +goose Up
ALTER TABLE namespace_entries ADD COLUMN source_event_hlc INTEGER;
ALTER TABLE namespace_entries ADD COLUMN source_event_device_id TEXT;
ALTER TABLE namespace_entries ADD COLUMN source_event_id TEXT;

-- +goose Down
ALTER TABLE namespace_entries DROP COLUMN source_event_id;
ALTER TABLE namespace_entries DROP COLUMN source_event_device_id;
ALTER TABLE namespace_entries DROP COLUMN source_event_hlc;
