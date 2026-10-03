-- +goose Up
CREATE TABLE backup_config (
  id TEXT PRIMARY KEY,
  backup_pool TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 0,
  schedule TEXT NOT NULL DEFAULT '',
  last_run_at TEXT
);
INSERT INTO backup_config (id, backup_pool, enabled, schedule, last_run_at)
  VALUES ('default', '', 0, '', NULL)
  ON CONFLICT(id) DO NOTHING;

CREATE TABLE backup_state (
  id TEXT PRIMARY KEY,
  source_name TEXT NOT NULL UNIQUE,
  last_snapshot TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE tasks_backup_dataset (
  id TEXT PRIMARY KEY,
  type TEXT NOT NULL CHECK (type IN ('import_image','create_config','delete_config','create_reduction','delete_reduction','merge_config','merge_reduction','super_stop','create_pool','destroy_pool','add_disk','migrate_disk','replace_disk','add_read_cache','remove_read_cache','add_write_cache','remove_write_cache','flush_cache','backup_dataset')),
  target_ref TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK (status IN ('pending','running','success','failed')),
  progress INTEGER NOT NULL DEFAULT 0 CHECK (progress >= 0 AND progress <= 100),
  result TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  finished_at TEXT
);
INSERT INTO tasks_backup_dataset (id,type,target_ref,status,progress,result,error,created_at,finished_at)
  SELECT id,type,target_ref,status,progress,result,error,created_at,finished_at FROM tasks;
DROP TABLE tasks;
ALTER TABLE tasks_backup_dataset RENAME TO tasks;

-- +goose Down
CREATE TABLE tasks_no_backup_dataset (
  id TEXT PRIMARY KEY,
  type TEXT NOT NULL CHECK (type IN ('import_image','create_config','delete_config','create_reduction','delete_reduction','merge_config','merge_reduction','super_stop','create_pool','destroy_pool','add_disk','migrate_disk','replace_disk','add_read_cache','remove_read_cache','add_write_cache','remove_write_cache','flush_cache')),
  target_ref TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK (status IN ('pending','running','success','failed')),
  progress INTEGER NOT NULL DEFAULT 0 CHECK (progress >= 0 AND progress <= 100),
  result TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  finished_at TEXT
);
INSERT INTO tasks_no_backup_dataset (id,type,target_ref,status,progress,result,error,created_at,finished_at)
  SELECT id,type,target_ref,status,progress,result,error,created_at,finished_at FROM tasks;
DROP TABLE tasks;
ALTER TABLE tasks_no_backup_dataset RENAME TO tasks;

DROP TABLE backup_state;
DROP TABLE backup_config;
