-- +goose Up
CREATE TABLE system_settings (
  id TEXT PRIMARY KEY,
  import_dir TEXT NOT NULL
);

-- +goose Down
DROP TABLE system_settings;
