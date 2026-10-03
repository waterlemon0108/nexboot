-- +goose Up
-- 数据盘在线发布是自己的一类任务：运维要能在任务列表里认出它、看到它失败的原因。
-- 和以往一样，tasks 重建一遍来放宽 CHECK。
CREATE TABLE tasks_publish (
  id TEXT PRIMARY KEY,
  type TEXT NOT NULL CHECK (type IN ('import_image','create_config','delete_config','create_reduction','delete_reduction','merge_config','merge_reduction','super_stop','create_pool','destroy_pool','add_disk','migrate_disk','replace_disk','add_read_cache','remove_read_cache','add_write_cache','remove_write_cache','flush_cache','backup_dataset','image_health_check','detach_disk','mirror_upgrade','add_special','remove_special','add_spare','remove_spare','create_blank_image','export_image','publish_data_disk')),
  target_ref TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK (status IN ('pending','running','success','failed')),
  progress INTEGER NOT NULL DEFAULT 0 CHECK (progress >= 0 AND progress <= 100),
  result TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  finished_at TEXT,
  message TEXT NOT NULL DEFAULT ''
);
INSERT INTO tasks_publish (id,type,target_ref,status,progress,result,error,created_at,finished_at,message)
  SELECT id,type,target_ref,status,progress,result,error,created_at,finished_at,message FROM tasks;
DROP TABLE tasks;
ALTER TABLE tasks_publish RENAME TO tasks;

-- +goose Down
CREATE TABLE tasks_no_publish (
  id TEXT PRIMARY KEY,
  type TEXT NOT NULL CHECK (type IN ('import_image','create_config','delete_config','create_reduction','delete_reduction','merge_config','merge_reduction','super_stop','create_pool','destroy_pool','add_disk','migrate_disk','replace_disk','add_read_cache','remove_read_cache','add_write_cache','remove_write_cache','flush_cache','backup_dataset','image_health_check','detach_disk','mirror_upgrade','add_special','remove_special','add_spare','remove_spare','create_blank_image','export_image')),
  target_ref TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK (status IN ('pending','running','success','failed')),
  progress INTEGER NOT NULL DEFAULT 0 CHECK (progress >= 0 AND progress <= 100),
  result TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  finished_at TEXT,
  message TEXT NOT NULL DEFAULT ''
);
INSERT INTO tasks_no_publish (id,type,target_ref,status,progress,result,error,created_at,finished_at,message)
  SELECT id,type,target_ref,status,progress,result,error,created_at,finished_at,message FROM tasks WHERE type <> 'publish_data_disk';
DROP TABLE tasks;
ALTER TABLE tasks_no_publish RENAME TO tasks;
