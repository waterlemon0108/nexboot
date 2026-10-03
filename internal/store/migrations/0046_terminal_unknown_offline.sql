-- +goose Up
-- 没连过的客户机按离线算：界面只有在线、离线两种状态。
UPDATE terminals SET state = 'offline' WHERE state = 'unknown';

-- +goose Down
SELECT 1;
