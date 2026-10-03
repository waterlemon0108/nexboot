-- +goose Up
ALTER TABLE system_settings ADD COLUMN client_iface TEXT NOT NULL DEFAULT '';
ALTER TABLE system_settings ADD COLUMN allow_cross_subnet INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE system_settings DROP COLUMN allow_cross_subnet;
ALTER TABLE system_settings DROP COLUMN client_iface;
