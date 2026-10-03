-- +goose Up
-- 任务记下执行它的节点。写入者的库会在切换时被下一台接过去，库里的历史任务不一定
-- 是当前这台跑的，事后推断不出来，只能创建时记住。旧任务留空。
ALTER TABLE tasks ADD COLUMN node TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE tasks DROP COLUMN node;
