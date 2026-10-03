-- +goose Up
ALTER TABLE tasks ADD COLUMN message TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE tasks DROP COLUMN message;
