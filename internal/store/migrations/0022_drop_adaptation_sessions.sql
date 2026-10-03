-- +goose Up
DROP TABLE IF EXISTS adaptation_sessions;

-- +goose Down
CREATE TABLE adaptation_sessions (
  id TEXT PRIMARY KEY,
  terminal_id TEXT NOT NULL REFERENCES terminals(id) ON DELETE CASCADE,
  bundle_id TEXT NOT NULL REFERENCES driver_bundles(id) ON DELETE CASCADE,
  token TEXT NOT NULL UNIQUE,
  token_expires_at TEXT NOT NULL,
  downloads_left INTEGER NOT NULL DEFAULT 3,
  status TEXT NOT NULL CHECK (status IN ('pending','downloaded','saved')),
  created_at TEXT NOT NULL
);
