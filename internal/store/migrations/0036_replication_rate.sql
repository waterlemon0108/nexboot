-- +goose Up
-- Runtime-adjustable replication rate cap. NULL means "not set, use the
-- deployment default"; 0 means explicitly unlimited — a maintenance window
-- with no clients running is exactly when an operator wants the cap off.
ALTER TABLE system_settings ADD COLUMN replication_rate_mbps INTEGER;

-- +goose Down
ALTER TABLE system_settings DROP COLUMN replication_rate_mbps;
