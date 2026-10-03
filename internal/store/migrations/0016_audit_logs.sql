-- +goose Up
CREATE TABLE audit_logs (
  id TEXT PRIMARY KEY,
  type TEXT NOT NULL CHECK (type IN ('operation','login')),
  username TEXT NOT NULL DEFAULT '',
  action TEXT NOT NULL DEFAULT '',
  module TEXT NOT NULL DEFAULT '',
  detail TEXT NOT NULL DEFAULT '',
  ip TEXT NOT NULL DEFAULT '',
  user_agent TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK (status IN ('ok','err')),
  http_status INTEGER NOT NULL DEFAULT 0,
  cost_ms INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL
);
CREATE INDEX idx_audit_logs_created ON audit_logs (created_at DESC);
CREATE INDEX idx_audit_logs_type ON audit_logs (type, created_at DESC);

-- +goose Down
DROP TABLE audit_logs;
