-- +goose Up
-- 每台机器最近一次开机失败，告警中心据此点名。
CREATE TABLE boot_failures (
  mac TEXT PRIMARY KEY,
  stage TEXT NOT NULL,
  code TEXT NOT NULL DEFAULT '',
  platform TEXT NOT NULL DEFAULT '',
  image_id TEXT NOT NULL DEFAULT '',
  at TEXT NOT NULL
);
-- 体检判出的引导方式，逗号分隔；空表示没判出来。
ALTER TABLE image_health_reports ADD COLUMN boot_modes TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE image_health_reports DROP COLUMN boot_modes;
DROP TABLE boot_failures;
