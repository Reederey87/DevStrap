-- +goose Up
-- needs_rotation marks a secret value that must be rotated at its source because
-- a device that could decrypt it was revoked/lost. Envelope re-encryption only
-- governs future reads; historical ciphertext stays decryptable by the revoked
-- key, so revocation is only complete once flagged values are rotated.
ALTER TABLE secret_bindings ADD COLUMN needs_rotation INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE secret_bindings DROP COLUMN needs_rotation;
