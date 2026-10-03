-- +goose Up
ALTER TABLE system_settings ADD COLUMN data_pool TEXT;

-- +goose Down
ALTER TABLE system_settings DROP COLUMN data_pool;
