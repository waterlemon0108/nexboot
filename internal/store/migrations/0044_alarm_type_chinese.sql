-- +goose Up
-- 复制滞后告警曾以英文 replication_lag 为类型写入，历史记录一并改成界面上的中文。
UPDATE alarms SET type = '复制滞后' WHERE type = 'replication_lag';

-- +goose Down
UPDATE alarms SET type = 'replication_lag' WHERE type = '复制滞后';
