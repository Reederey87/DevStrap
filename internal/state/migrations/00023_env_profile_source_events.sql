-- +goose Up
-- ENV-SYNC-01: stamp env profiles with the source event coordinate so
-- cross-device env.profile.updated replay is idempotent and LWW-convergent.
ALTER TABLE env_profiles ADD COLUMN source_event_hlc INTEGER;
ALTER TABLE env_profiles ADD COLUMN source_event_device_id TEXT;
ALTER TABLE env_profiles ADD COLUMN source_event_id TEXT;

-- +goose Down
ALTER TABLE env_profiles DROP COLUMN source_event_id;
ALTER TABLE env_profiles DROP COLUMN source_event_device_id;
ALTER TABLE env_profiles DROP COLUMN source_event_hlc;
