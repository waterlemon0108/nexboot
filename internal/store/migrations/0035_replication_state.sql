-- +goose Up
-- One row per replication target (the HA peer today; more under fan-out).
-- The row is bookkeeping for operators and alarms — the true incremental base
-- is read from the pools themselves each round.
CREATE TABLE replication_state (
  target TEXT PRIMARY KEY,
  kind TEXT NOT NULL DEFAULT 'standby-pull',
  root TEXT NOT NULL DEFAULT '',
  last_snapshot TEXT NOT NULL DEFAULT '',
  last_ok_at TEXT,
  last_error TEXT NOT NULL DEFAULT '',
  updated_at TEXT
);

-- +goose Down
DROP TABLE replication_state;
