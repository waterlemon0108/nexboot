-- +goose Up
CREATE TABLE alarms (
  id TEXT PRIMARY KEY,
  alarm_key TEXT NOT NULL,
  severity TEXT NOT NULL CHECK (severity IN ('info','warn','error')),
  type TEXT NOT NULL,
  source TEXT NOT NULL DEFAULT '',
  resource TEXT NOT NULL DEFAULT '',
  threshold TEXT NOT NULL DEFAULT '',
  value TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK (status IN ('active','acknowledged','recovered')),
  message TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  recovered_at TEXT
);
CREATE INDEX idx_alarms_status ON alarms (status, updated_at DESC);
-- At most one open (active/acknowledged) alarm per condition key; recovered
-- entries accumulate as history.
CREATE UNIQUE INDEX idx_alarms_open_key ON alarms (alarm_key) WHERE status IN ('active','acknowledged');

-- +goose Down
DROP TABLE alarms;
