-- +goose Up
-- Records which startup-script version an image was baked with at import, so
-- the boot path can skip the per-client injection without mounting anything.
-- Existing images get '' and keep injecting per client.
ALTER TABLE images ADD COLUMN mount_script_version TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE images DROP COLUMN mount_script_version;
