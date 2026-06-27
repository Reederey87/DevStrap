-- +goose Up
ALTER TABLE devices ADD COLUMN signing_public_key TEXT;

-- +goose Down
ALTER TABLE devices DROP COLUMN signing_public_key;
