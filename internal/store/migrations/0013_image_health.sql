-- +goose Up
CREATE TABLE image_health_reports (
  id TEXT PRIMARY KEY,
  image_id TEXT NOT NULL UNIQUE REFERENCES images(id) ON DELETE CASCADE,
  level TEXT NOT NULL CHECK (level IN ('ok','warn','block','unknown')),
  partition_style TEXT NOT NULL DEFAULT '',
  items TEXT NOT NULL DEFAULT '[]',
  created_at TEXT NOT NULL
);

CREATE TABLE tasks_image_health (
  id TEXT PRIMARY KEY,
  type TEXT NOT NULL CHECK (type IN ('import_image','create_config','delete_config','create_reduction','delete_reduction','merge_config','merge_reduction','super_stop','create_pool','destroy_pool','add_disk','migrate_disk','replace_disk','add_read_cache','remove_read_cache','add_write_cache','remove_write_cache','flush_cache','backup_dataset','image_health_check')),
  target_ref TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK (status IN ('pending','running','success','failed')),
  progress INTEGER NOT NULL DEFAULT 0 CHECK (progress >= 0 AND progress <= 100),
  result TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  finished_at TEXT,
  message TEXT NOT NULL DEFAULT ''
);
INSERT INTO tasks_image_health (id,type,target_ref,status,progress,result,error,created_at,finished_at,message)
  SELECT id,type,target_ref,status,progress,result,error,created_at,finished_at,message FROM tasks;
DROP TABLE tasks;
ALTER TABLE tasks_image_health RENAME TO tasks;

-- +goose Down
CREATE TABLE tasks_no_image_health (
  id TEXT PRIMARY KEY,
  type TEXT NOT NULL CHECK (type IN ('import_image','create_config','delete_config','create_reduction','delete_reduction','merge_config','merge_reduction','super_stop','create_pool','destroy_pool','add_disk','migrate_disk','replace_disk','add_read_cache','remove_read_cache','add_write_cache','remove_write_cache','flush_cache','backup_dataset')),
  target_ref TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK (status IN ('pending','running','success','failed')),
  progress INTEGER NOT NULL DEFAULT 0 CHECK (progress >= 0 AND progress <= 100),
  result TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  finished_at TEXT,
  message TEXT NOT NULL DEFAULT ''
);
INSERT INTO tasks_no_image_health (id,type,target_ref,status,progress,result,error,created_at,finished_at,message)
  SELECT id,type,target_ref,status,progress,result,error,created_at,finished_at,message
  FROM tasks WHERE type <> 'image_health_check';
DROP TABLE tasks;
ALTER TABLE tasks_no_image_health RENAME TO tasks;

DROP TABLE image_health_reports;
