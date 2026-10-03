-- +goose Up
ALTER TABLE terminals ADD COLUMN name TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE terminals DROP COLUMN name;
