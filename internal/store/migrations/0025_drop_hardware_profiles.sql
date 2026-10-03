-- +goose Up
-- 硬件画像整体下线：采集端每次开机免费归并，但下游（兼容矩阵 FR-F5/F6）已于
-- 0021 一并移除，此后没有任何流程读它——只剩采集、列表与改名。
ALTER TABLE terminals DROP COLUMN hardware_profile_id;
DROP TABLE IF EXISTS hardware_profiles;

-- +goose Down
CREATE TABLE hardware_profiles (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  fingerprint TEXT NOT NULL UNIQUE,
  boot_mode TEXT NOT NULL DEFAULT '',
  nic_pci_id TEXT NOT NULL DEFAULT '',
  board_vendor TEXT NOT NULL DEFAULT '',
  board_model TEXT NOT NULL DEFAULT '',
  device_hwids TEXT NOT NULL DEFAULT '[]',
  probe_level TEXT NOT NULL DEFAULT 'l1' CHECK (probe_level IN ('l1','l2')),
  created_at TEXT NOT NULL
);
ALTER TABLE terminals ADD COLUMN hardware_profile_id TEXT REFERENCES hardware_profiles(id);
