-- +goose Up
-- An image says what it is for (system disk / data disk) and where it came
-- from (imported / created blank). Every image so far is an imported system
-- disk — the only kind there was. Kept as ADD COLUMN: images has children.
ALTER TABLE images ADD COLUMN purpose TEXT NOT NULL DEFAULT 'system' CHECK (purpose IN ('system','data'));
ALTER TABLE images ADD COLUMN origin TEXT NOT NULL DEFAULT 'imported' CHECK (origin IN ('imported','blank'));

-- The blank-data-disk creation is a task of its own; tasks is rebuilt to
-- widen the CHECK, as before.
CREATE TABLE tasks_blank (
  id TEXT PRIMARY KEY,
  type TEXT NOT NULL CHECK (type IN ('import_image','create_config','delete_config','create_reduction','delete_reduction','merge_config','merge_reduction','super_stop','create_pool','destroy_pool','add_disk','migrate_disk','replace_disk','add_read_cache','remove_read_cache','add_write_cache','remove_write_cache','flush_cache','backup_dataset','image_health_check','detach_disk','mirror_upgrade','add_special','remove_special','add_spare','remove_spare','create_blank_image')),
  target_ref TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK (status IN ('pending','running','success','failed')),
  progress INTEGER NOT NULL DEFAULT 0 CHECK (progress >= 0 AND progress <= 100),
  result TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  finished_at TEXT,
  message TEXT NOT NULL DEFAULT ''
);
INSERT INTO tasks_blank (id,type,target_ref,status,progress,result,error,created_at,finished_at,message)
  SELECT id,type,target_ref,status,progress,result,error,created_at,finished_at,message FROM tasks;
DROP TABLE tasks;
ALTER TABLE tasks_blank RENAME TO tasks;

-- +goose Down
CREATE TABLE tasks_no_blank (
  id TEXT PRIMARY KEY,
  type TEXT NOT NULL CHECK (type IN ('import_image','create_config','delete_config','create_reduction','delete_reduction','merge_config','merge_reduction','super_stop','create_pool','destroy_pool','add_disk','migrate_disk','replace_disk','add_read_cache','remove_read_cache','add_write_cache','remove_write_cache','flush_cache','backup_dataset','image_health_check','detach_disk','mirror_upgrade','add_special','remove_special','add_spare','remove_spare')),
  target_ref TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK (status IN ('pending','running','success','failed')),
  progress INTEGER NOT NULL DEFAULT 0 CHECK (progress >= 0 AND progress <= 100),
  result TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  finished_at TEXT,
  message TEXT NOT NULL DEFAULT ''
);
INSERT INTO tasks_no_blank (id,type,target_ref,status,progress,result,error,created_at,finished_at,message)
  SELECT id,type,target_ref,status,progress,result,error,created_at,finished_at,message FROM tasks WHERE type <> 'create_blank_image';
DROP TABLE tasks;
ALTER TABLE tasks_no_blank RENAME TO tasks;
ALTER TABLE images DROP COLUMN origin;
ALTER TABLE images DROP COLUMN purpose;
