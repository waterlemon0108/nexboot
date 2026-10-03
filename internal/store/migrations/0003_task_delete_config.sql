-- +goose Up
CREATE TABLE tasks_next (
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
INSERT INTO tasks_next (id,type,target_ref,status,progress,result,error,created_at,finished_at)
  SELECT id,type,target_ref,status,progress,result,error,created_at,finished_at FROM tasks;
DROP TABLE tasks;
ALTER TABLE tasks_next RENAME TO tasks;

-- +goose Down
CREATE TABLE tasks_prev (
  id TEXT PRIMARY KEY,
  type TEXT NOT NULL CHECK (type IN ('import_image','create_config','create_reduction','merge_config','merge_reduction','super_stop','add_disk','migrate_disk','flush_cache')),
  target_ref TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK (status IN ('pending','running','success','failed')),
  progress INTEGER NOT NULL DEFAULT 0 CHECK (progress >= 0 AND progress <= 100),
  result TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  finished_at TEXT
);
INSERT INTO tasks_prev (id,type,target_ref,status,progress,result,error,created_at,finished_at)
  SELECT id,type,target_ref,status,progress,result,error,created_at,finished_at FROM tasks
  WHERE type <> 'delete_config';
DROP TABLE tasks;
ALTER TABLE tasks_prev RENAME TO tasks;
