-- +goose Up
CREATE TABLE images (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  os_type TEXT NOT NULL CHECK (os_type IN ('windows','linux')),
  size INTEGER NOT NULL DEFAULT 0 CHECK (size >= 0),
  state TEXT NOT NULL CHECK (state IN ('normal','importing','error')),
  remark TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);

CREATE TABLE configs (
  id TEXT PRIMARY KEY,
  image_id TEXT NOT NULL REFERENCES images(id) ON DELETE RESTRICT,
  name TEXT NOT NULL,
  default_reduction_id TEXT,
  created_at TEXT NOT NULL,
  UNIQUE (image_id, name)
);

CREATE TABLE reductions (
  id TEXT PRIMARY KEY,
  config_id TEXT NOT NULL REFERENCES configs(id) ON DELETE RESTRICT,
  name TEXT NOT NULL,
  created_at TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('ready','creating','error')),
  remark TEXT NOT NULL DEFAULT '',
  UNIQUE (config_id, name)
);

CREATE TABLE groups (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  is_default BOOLEAN NOT NULL DEFAULT FALSE,
  start_ip TEXT NOT NULL,
  client_max INTEGER NOT NULL CHECK (client_max > 0),
  gateway TEXT NOT NULL,
  netmask TEXT NOT NULL,
  dns1 TEXT NOT NULL DEFAULT '',
  dns2 TEXT NOT NULL DEFAULT '',
  system_image_id TEXT NOT NULL REFERENCES images(id) ON DELETE RESTRICT,
  system_config_id TEXT NOT NULL REFERENCES configs(id) ON DELETE RESTRICT,
  system_reduction_id TEXT NOT NULL REFERENCES reductions(id) ON DELETE RESTRICT
);

CREATE UNIQUE INDEX one_default_group ON groups (is_default) WHERE is_default;

CREATE TABLE group_disks (
  id TEXT PRIMARY KEY,
  group_id TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
  mount_target TEXT NOT NULL,
  image_id TEXT NOT NULL REFERENCES images(id) ON DELETE RESTRICT,
  config_id TEXT NOT NULL REFERENCES configs(id) ON DELETE RESTRICT,
  UNIQUE (group_id, mount_target)
);

CREATE TABLE terminals (
  id TEXT PRIMARY KEY,
  mac TEXT NOT NULL UNIQUE,
  ip TEXT NOT NULL UNIQUE,
  group_id TEXT NOT NULL REFERENCES groups(id) ON DELETE RESTRICT,
  is_super BOOLEAN NOT NULL DEFAULT FALSE,
  state TEXT NOT NULL CHECK (state IN ('online','offline','unknown')),
  online_since TEXT,
  offline_at TEXT,
  last_heartbeat_at TEXT
);

CREATE TABLE servers (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  ip TEXT NOT NULL UNIQUE,
  role TEXT NOT NULL CHECK (role IN ('control','storage','all')),
  status TEXT NOT NULL CHECK (status IN ('up','down')),
  heartbeat TEXT
);

CREATE TABLE pools (
  id TEXT PRIMARY KEY,
  server_id TEXT NOT NULL REFERENCES servers(id) ON DELETE RESTRICT,
  name TEXT NOT NULL,
  disks TEXT NOT NULL DEFAULT '[]',
  read_cache_disks TEXT NOT NULL DEFAULT '[]',
  write_cache_disks TEXT NOT NULL DEFAULT '[]',
  capacity INTEGER NOT NULL DEFAULT 0 CHECK (capacity >= 0),
  used INTEGER NOT NULL DEFAULT 0 CHECK (used >= 0),
  UNIQUE (server_id, name)
);

CREATE TABLE pool_disks (
  id TEXT PRIMARY KEY,
  pool_id TEXT NOT NULL REFERENCES pools(id) ON DELETE CASCADE,
  path TEXT NOT NULL,
  role TEXT NOT NULL CHECK (role IN ('data','read_cache','write_cache')),
  capacity INTEGER NOT NULL DEFAULT 0 CHECK (capacity >= 0),
  used INTEGER NOT NULL DEFAULT 0 CHECK (used >= 0),
  UNIQUE (pool_id, path, role)
);

CREATE TABLE client_clones (
  id TEXT PRIMARY KEY,
  terminal_mac TEXT NOT NULL REFERENCES terminals(mac) ON DELETE CASCADE,
  kind TEXT NOT NULL CHECK (kind IN ('ephemeral','persistent')),
  config_id TEXT NOT NULL REFERENCES configs(id) ON DELETE RESTRICT,
  reduction_id TEXT NOT NULL REFERENCES reductions(id) ON DELETE RESTRICT,
  server_id TEXT NOT NULL REFERENCES servers(id) ON DELETE RESTRICT,
  target TEXT NOT NULL,
  lun INTEGER NOT NULL CHECK (lun >= 0),
  volpath TEXT NOT NULL,
  UNIQUE (terminal_mac, kind, target, lun)
);

CREATE TABLE tasks (
  id TEXT PRIMARY KEY,
  type TEXT NOT NULL CHECK (type IN ('import_image','create_config','delete_config','create_reduction','merge_config','merge_reduction','super_stop','add_disk','migrate_disk','flush_cache')),
  target_ref TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK (status IN ('pending','running','success','failed')),
  progress INTEGER NOT NULL DEFAULT 0 CHECK (progress >= 0 AND progress <= 100),
  result TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  finished_at TEXT
);

CREATE TABLE users (
  id TEXT PRIMARY KEY,
  username TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  created_at TEXT NOT NULL
);

-- +goose Down
DROP TABLE users;
DROP TABLE tasks;
DROP TABLE client_clones;
DROP TABLE pool_disks;
DROP TABLE pools;
DROP TABLE servers;
DROP TABLE terminals;
DROP TABLE group_disks;
DROP TABLE groups;
DROP TABLE reductions;
DROP TABLE configs;
DROP TABLE images;
