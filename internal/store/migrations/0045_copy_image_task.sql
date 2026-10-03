-- +goose Up
-- 另存为新镜像是自己的一类任务，任务列表里要认得出来。与以往一样重建 tasks 放宽 CHECK。
CREATE TABLE tasks_copy (
  id TEXT PRIMARY KEY,
  type TEXT NOT NULL CHECK (type IN ('import_image','create_config','delete_config','create_reduction','delete_reduction','merge_config','merge_reduction','super_stop','create_pool','destroy_pool','add_disk','migrate_disk','replace_disk','add_read_cache','remove_read_cache','add_write_cache','remove_write_cache','flush_cache','backup_dataset','image_health_check','detach_disk','mirror_upgrade','add_special','remove_special','add_spare','remove_spare','create_blank_image','export_image','publish_data_disk','copy_image')),
  target_ref TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK (status IN ('pending','running','success','failed')),
  progress INTEGER NOT NULL DEFAULT 0 CHECK (progress >= 0 AND progress <= 100),
  result TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  finished_at TEXT,
  message TEXT NOT NULL DEFAULT '',
  node TEXT NOT NULL DEFAULT ''
);
INSERT INTO tasks_copy (id,type,target_ref,status,progress,result,error,created_at,finished_at,message,node)
  SELECT id,type,target_ref,status,progress,result,error,created_at,finished_at,message,node FROM tasks;
DROP TABLE tasks;
ALTER TABLE tasks_copy RENAME TO tasks;

-- +goose Down
CREATE TABLE tasks_no_copy (
  id TEXT PRIMARY KEY,
  type TEXT NOT NULL CHECK (type IN ('import_image','create_config','delete_config','create_reduction','delete_reduction','merge_config','merge_reduction','super_stop','create_pool','destroy_pool','add_disk','migrate_disk','replace_disk','add_read_cache','remove_read_cache','add_write_cache','remove_write_cache','flush_cache','backup_dataset','image_health_check','detach_disk','mirror_upgrade','add_special','remove_special','add_spare','remove_spare','create_blank_image','export_image','publish_data_disk')),
  target_ref TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK (status IN ('pending','running','success','failed')),
  progress INTEGER NOT NULL DEFAULT 0 CHECK (progress >= 0 AND progress <= 100),
  result TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  finished_at TEXT,
  message TEXT NOT NULL DEFAULT '',
  node TEXT NOT NULL DEFAULT ''
);
INSERT INTO tasks_no_copy (id,type,target_ref,status,progress,result,error,created_at,finished_at,message,node)
  SELECT id,type,target_ref,status,progress,result,error,created_at,finished_at,message,node FROM tasks WHERE type <> 'copy_image';
DROP TABLE tasks;
ALTER TABLE tasks_no_copy RENAME TO tasks;
