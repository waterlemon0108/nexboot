-- +goose Up
CREATE TABLE driver_packs (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  category TEXT NOT NULL CHECK (category IN ('boot_critical_nic','nic','gpu','audio','chipset','other')),
  os_type TEXT NOT NULL CHECK (os_type IN ('windows','linux')),
  arch TEXT NOT NULL DEFAULT 'x64',
  version TEXT NOT NULL DEFAULT '',
  release_date TEXT NOT NULL DEFAULT '',
  vendor TEXT NOT NULL DEFAULT '',
  signed BOOLEAN NOT NULL DEFAULT FALSE,
  hwids TEXT NOT NULL DEFAULT '[]',
  status TEXT NOT NULL CHECK (status IN ('enabled','disabled')),
  recommended BOOLEAN NOT NULL DEFAULT FALSE,
  storage_path TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE driver_bundles (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  os_type TEXT NOT NULL CHECK (os_type IN ('windows','linux')),
  created_at TEXT NOT NULL
);

CREATE TABLE driver_bundle_packs (
  id TEXT PRIMARY KEY,
  bundle_id TEXT NOT NULL REFERENCES driver_bundles(id) ON DELETE CASCADE,
  pack_id TEXT NOT NULL REFERENCES driver_packs(id) ON DELETE RESTRICT,
  UNIQUE (bundle_id, pack_id)
);

-- +goose Down
DROP TABLE driver_bundle_packs;
DROP TABLE driver_bundles;
DROP TABLE driver_packs;
