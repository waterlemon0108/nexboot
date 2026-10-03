-- +goose Up
-- The per-group choice lasted an afternoon: a group binds image + config, and
-- the restore point is decided on the config (apply) for every group at once.
ALTER TABLE groups DROP COLUMN follow_current;

-- +goose Down
ALTER TABLE groups ADD COLUMN follow_current INTEGER NOT NULL DEFAULT 1;
